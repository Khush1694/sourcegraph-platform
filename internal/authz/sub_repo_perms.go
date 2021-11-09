package authz

import (
	"context"
	"path"
	"sync/atomic"

	"github.com/cockroachdb/errors"
	"github.com/gobwas/glob"

	"github.com/sourcegraph/sourcegraph/internal/actor"
	"github.com/sourcegraph/sourcegraph/internal/api"
	"github.com/sourcegraph/sourcegraph/internal/conf"
)

var (
	subRepoPermsEnabledFlag int32
)

func init() {
	// We need to check whether sub-repo permissions are enabled often so we cache
	// the config value locally and only update it when it changes.
	//
	// This will be removed once sub-repo perms are no longer experimental.
	go conf.Watch(func() {
		c := conf.Get()
		if c.ExperimentalFeatures != nil && c.ExperimentalFeatures.EnableSubRepoPermissions {
			atomic.StoreInt32(&subRepoPermsEnabledFlag, 1)
		} else {
			atomic.StoreInt32(&subRepoPermsEnabledFlag, 0)
		}
	})
}

func subRepoPermsEnabled() bool {
	return atomic.LoadInt32(&subRepoPermsEnabledFlag) == 1
}

// RepoContent specifies data existing in a repo. It currently only supports
// paths but will be extended in future to support other pieces of metadata, for
// example branch.
type RepoContent struct {
	Repo api.RepoName
	Path string
}

// SubRepoPermissionChecker is the interface exposed by the SubRepoPermsClient and is
// exposed to allow consumers to mock out the client.
//
//go:generate ../../dev/mockgen.sh github.com/sourcegraph/sourcegraph/internal/authz -i SubRepoPermissionChecker -o mock_sub_repo_perms.go
type SubRepoPermissionChecker interface {
	// Permissions returns the level of access the provided user has for the requested
	// content.
	//
	// If the userID represents an anonymous user, ErrUnauthenticated is returned.
	Permissions(ctx context.Context, userID int32, content RepoContent) (Perms, error)
}

var _ SubRepoPermissionChecker = &subRepoPermsClient{}

// SubRepoPermissionsGetter allows getting sub repository permissions.
type SubRepoPermissionsGetter interface {
	// GetByUser returns the known sub repository permissions rules known for a user.
	GetByUser(ctx context.Context, userID int32) (map[api.RepoName]SubRepoPermissions, error)

	// RepoSupported should be used to quickly check whether sub-repo permissions are
	// supported for the given repo.
	RepoSupported(ctx context.Context, repo api.RepoName) (bool, error)
}

// subRepoPermsClient is a concrete implementation of SubRepoPermissionChecker.
type subRepoPermsClient struct {
	permissionsGetter SubRepoPermissionsGetter
}

// NewSubRepoPermsClient instantiates an instance of authz.SubRepoPermissionChecker.
//
// SubRepoPermissionChecker is responsible for checking whether a user has access to
// data within a repo. Sub-repository permissions enforcement is on top of existing
// repository permissions, which means the user must already have access to the
// repository itself. The intention is for this client to be created once at startup
// and passed in to all places that need to check sub repo permissions.
//
// Note that sub-repo permissions are currently opt-in via the
// experimentalFeatures.enableSubRepoPermissions option.
func NewSubRepoPermsClient(permissionsGetter SubRepoPermissionsGetter) *subRepoPermsClient {
	return &subRepoPermsClient{
		permissionsGetter: permissionsGetter,
	}
}

func (s *subRepoPermsClient) Permissions(ctx context.Context, userID int32, content RepoContent) (Perms, error) {
	// Are sub-repo permissions enabled at the site level
	if !subRepoPermsEnabled() {
		return Read, nil
	}

	if userID == 0 {
		return None, &ErrUnauthenticated{}
	}

	if s.permissionsGetter == nil {
		return None, errors.New("PermissionsGetter is nil")
	}

	if supported, err := s.permissionsGetter.RepoSupported(ctx, content.Repo); err != nil {
		return None, errors.Wrap(err, "checking for sub-repo permissions support")
	} else if !supported {
		// We assume that repo level access has already been granted
		return Read, nil
	}

	srp, err := s.permissionsGetter.GetByUser(ctx, userID)
	if err != nil {
		return None, errors.Wrap(err, "getting permissions")
	}

	// Check repo
	repoRules, ok := srp[content.Repo]
	if !ok {
		// All repos that support sub-repo permissions should at the very least have an
		// "allow all" rule. If no rules exist it implies that we haven't performed a
		// permissions sync yet and it is safer to assume no access is allowed.
		return None, nil
	}

	// TODO: This will be very slow until we can cache compiled rules
	includeMatchers := make([]glob.Glob, 0, len(repoRules.PathIncludes))
	for _, rule := range repoRules.PathIncludes {
		g, err := glob.Compile(rule, '/')
		if err != nil {
			return None, errors.Wrap(err, "building include matcher")
		}
		includeMatchers = append(includeMatchers, g)
	}
	excludeMatchers := make([]glob.Glob, 0, len(repoRules.PathExcludes))
	for _, rule := range repoRules.PathExcludes {
		g, err := glob.Compile(rule, '/')
		if err != nil {
			return None, errors.Wrap(err, "building exclude matcher")
		}
		excludeMatchers = append(excludeMatchers, g)
	}

	// Rules are created including the repo name
	toMatch := path.Join(string(content.Repo), content.Path)

	// The current path needs to either be included or NOT excluded and we'll give
	// preference to exclusion.
	for _, rule := range excludeMatchers {
		if rule.Match(toMatch) {
			return None, nil
		}
	}
	for _, rule := range includeMatchers {
		if rule.Match(toMatch) {
			return Read, nil
		}
	}

	// Return None if no rule matches to be safe
	return None, nil
}

// CurrentUserPermissions returns the level of access the authenticated user within
// the provided context has for the requested content by calling ActorPermissions.
func CurrentUserPermissions(ctx context.Context, s SubRepoPermissionChecker, content RepoContent) (Perms, error) {
	return ActorPermissions(ctx, s, actor.FromContext(ctx), content)
}

// ActorPermissions returns the level of access the given actor has for the requested
// content.
//
// If the context is unauthenticated, ErrUnauthenticated is returned. If the context is
// internal, Read permissions is granted.
func ActorPermissions(ctx context.Context, s SubRepoPermissionChecker, a *actor.Actor, content RepoContent) (Perms, error) {
	// Check config here, despite checking again in the s.Permissions implementation,
	// because we also make some permissions decisions here.
	if !subRepoPermsEnabled() {
		return Read, nil
	}

	if !a.IsAuthenticated() {
		return None, &ErrUnauthenticated{}
	}
	if a.IsInternal() {
		return Read, nil
	}

	return s.Permissions(ctx, a.UID, content)
}