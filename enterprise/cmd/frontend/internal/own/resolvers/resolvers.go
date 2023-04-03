// Ownership resolvers are currently just returning fake data to support development.
// The actual resolver implementation is landing with #46592.
package resolvers

import (
	"context"
	"fmt"
	"sort"

	"github.com/grafana/regexp"
	"github.com/graph-gophers/graphql-go"
	"github.com/graph-gophers/graphql-go/relay"

	"github.com/sourcegraph/log"

	"github.com/sourcegraph/sourcegraph/cmd/frontend/graphqlbackend"
	"github.com/sourcegraph/sourcegraph/cmd/frontend/graphqlbackend/graphqlutil"
	edb "github.com/sourcegraph/sourcegraph/enterprise/internal/database"
	"github.com/sourcegraph/sourcegraph/enterprise/internal/own"
	"github.com/sourcegraph/sourcegraph/enterprise/internal/own/codeowners"
	codeownerspb "github.com/sourcegraph/sourcegraph/enterprise/internal/own/codeowners/v1"
	"github.com/sourcegraph/sourcegraph/internal/api"
	"github.com/sourcegraph/sourcegraph/internal/database"
	"github.com/sourcegraph/sourcegraph/internal/featureflag"
	"github.com/sourcegraph/sourcegraph/internal/gitserver"
	"github.com/sourcegraph/sourcegraph/lib/errors"
)

func New(db database.DB, gitserver gitserver.Client, logger log.Logger) graphqlbackend.OwnResolver {
	return &ownResolver{
		db:           edb.NewEnterpriseDB(db),
		gitserver:    gitserver,
		ownServiceFn: func() own.Service { return own.NewService(gitserver, db) },
		logger:       logger,
	}
}

func NewWithService(db database.DB, gitserver gitserver.Client, ownService own.Service, logger log.Logger) graphqlbackend.OwnResolver {
	return &ownResolver{
		db:           edb.NewEnterpriseDB(db),
		gitserver:    gitserver,
		ownServiceFn: func() own.Service { return ownService },
		logger:       logger,
	}
}

var (
	_ graphqlbackend.OwnResolver = &ownResolver{}
)

type ownResolver struct {
	db           edb.EnterpriseDB
	gitserver    gitserver.Client
	ownServiceFn func() own.Service
	logger       log.Logger
}

func (r *ownResolver) ownService() own.Service {
	return r.ownServiceFn()
}

func ownerText(o *codeownerspb.Owner) string {
	if o == nil {
		return ""
	}
	if o.Handle != "" {
		return o.Handle
	}
	return o.Email
}

func (r *ownResolver) GitBlobOwnership(
	ctx context.Context,
	blob *graphqlbackend.GitTreeEntryResolver,
	args graphqlbackend.ListOwnershipArgs,
) (graphqlbackend.OwnershipConnectionResolver, error) {
	if err := areOwnEndpointsAvailable(ctx); err != nil {
		return nil, err
	}
	cursor, err := graphqlutil.DecodeCursor(args.After)
	if err != nil {
		return nil, err
	}
	repo := blob.Repository()
	repoID, repoName := repo.IDInt32(), repo.RepoName()
	commitID := api.CommitID(blob.Commit().OID())
	ownService := r.ownService()
	rs, err := ownService.RulesetForRepo(ctx, repoName, repoID, commitID)
	if err != nil {
		return nil, err
	}
	// No ruleset found.
	if rs == nil {
		return &ownershipConnectionResolver{db: r.db}, nil
	}
	rule := rs.Match(blob.Path())
	// No match found.
	if rule == nil {
		return &ownershipConnectionResolver{db: r.db}, nil
	}
	owners := rule.GetOwner()
	sort.Slice(owners, func(i, j int) bool {
		iText := ownerText(owners[i])
		jText := ownerText(owners[j])
		return iText < jText
	})
	total := len(owners)
	for cursor != "" && len(owners) > 0 && ownerText(owners[0]) != cursor {
		owners = owners[1:]
	}
	var next *string
	if args.First != nil && len(owners) > int(*args.First) {
		cursor := ownerText(owners[*args.First])
		next = &cursor
		owners = owners[:*args.First]
	}
	resolvedOwners, err := ownService.ResolveOwnersWithType(ctx, owners)
	if err != nil {
		return nil, err
	}
	ownerships := make([]graphqlbackend.OwnershipResolver, 0, len(resolvedOwners))
	for _, ro := range resolvedOwners {
		reasons := []graphqlbackend.OwnershipReasonResolver{
			&codeownersFileEntryResolver{
				db:              r.db,
				gitserverClient: r.gitserver,
				source:          rs.GetSource(),
				repo:            blob.Repository(),
				matchLineNumber: rule.GetLineNumber(),
			},
		}
		ownerships = append(ownerships, &ownershipResolver{
			db:            r.db,
			resolvedOwner: ro,
			reasons:       reasons,
		})
	}
	return &ownershipConnectionResolver{
		db:             r.db,
		total:          total,
		next:           next,
		resolvedOwners: resolvedOwners,
		ownerships:     ownerships,
	}, nil
}

func (r *ownResolver) PersonOwnerField(person *graphqlbackend.PersonResolver) string {
	return "owner"
}

func (r *ownResolver) UserOwnerField(user *graphqlbackend.UserResolver) string {
	return "owner"
}

func (r *ownResolver) TeamOwnerField(team *graphqlbackend.TeamResolver) string {
	return "owner"
}

func (r *ownResolver) NodeResolvers() map[string]graphqlbackend.NodeByIDFunc {
	return map[string]graphqlbackend.NodeByIDFunc{
		codeownersIngestedFileKind: func(ctx context.Context, id graphql.ID) (graphqlbackend.Node, error) {
			// codeowners ingested files are identified by repo ID at the moment.
			var repoID api.RepoID
			if err := relay.UnmarshalSpec(id, &repoID); err != nil {
				return nil, errors.Wrap(err, "could not unmarshal repository ID")
			}
			return r.RepoIngestedCodeowners(ctx, repoID)
		},
	}
}

type ownershipConnectionResolver struct {
	db             edb.EnterpriseDB
	total          int
	next           *string
	resolvedOwners []codeowners.ResolvedOwner
	ownerships     []graphqlbackend.OwnershipResolver
}

func (r *ownershipConnectionResolver) TotalCount(_ context.Context) (int32, error) {
	return int32(r.total), nil
}

func (r *ownershipConnectionResolver) PageInfo(_ context.Context) (*graphqlutil.PageInfo, error) {
	return graphqlutil.EncodeCursor(r.next), nil
}

func (r *ownershipConnectionResolver) Nodes(_ context.Context) ([]graphqlbackend.OwnershipResolver, error) {
	return r.ownerships, nil
}

type ownershipResolver struct {
	db            edb.EnterpriseDB
	resolvedOwner codeowners.ResolvedOwner
	reasons       []graphqlbackend.OwnershipReasonResolver
}

func (r *ownershipResolver) Owner(ctx context.Context) (graphqlbackend.OwnerResolver, error) {
	if err := areOwnEndpointsAvailable(ctx); err != nil {
		return nil, err
	}
	return &ownerResolver{
		db:            r.db,
		resolvedOwner: r.resolvedOwner,
	}, nil
}

func (r *ownershipResolver) Reasons(_ context.Context) ([]graphqlbackend.OwnershipReasonResolver, error) {
	return r.reasons, nil
}

type ownerResolver struct {
	db            database.DB
	resolvedOwner codeowners.ResolvedOwner
}

func (r *ownerResolver) OwnerField(_ context.Context) (string, error) { return "owner", nil }

func (r *ownerResolver) ToPerson() (*graphqlbackend.PersonResolver, bool) {
	if r.resolvedOwner.Type() != codeowners.OwnerTypePerson {
		return nil, false
	}
	person, ok := r.resolvedOwner.(*codeowners.Person)
	if !ok {
		return nil, false
	}
	includeUserInfo := true
	return graphqlbackend.NewPersonResolver(r.db, person.Handle, person.GetEmail(), includeUserInfo), true
}

func (r *ownerResolver) ToTeam() (*graphqlbackend.TeamResolver, bool) {
	if r.resolvedOwner.Type() != codeowners.OwnerTypeTeam {
		return nil, false
	}
	resolvedTeam, ok := r.resolvedOwner.(*codeowners.Team)
	if !ok {
		return nil, false
	}
	return graphqlbackend.NewTeamResolver(r.db, resolvedTeam.Team), true
}

type codeownersFileEntryResolver struct {
	db              edb.EnterpriseDB
	source          codeowners.RulesetSource
	matchLineNumber int32
	repo            *graphqlbackend.RepositoryResolver
	gitserverClient gitserver.Client
}

func (r *codeownersFileEntryResolver) ToCodeownersFileEntry() (graphqlbackend.CodeownersFileEntryResolver, bool) {
	return r, true
}

func (r *codeownersFileEntryResolver) Title(_ context.Context) (string, error) {
	return "CODEOWNERS", nil
}

func (r *codeownersFileEntryResolver) Description(_ context.Context) (string, error) {
	return "Owner is associated with a rule in a CODEOWNERS file.", nil
}

func (r *codeownersFileEntryResolver) CodeownersFile(ctx context.Context) (graphqlbackend.FileResolver, error) {
	switch src := r.source.(type) {
	case codeowners.IngestedRulesetSource:
		// For ingested, create a virtual file resolver that loads the raw contents
		// on demand.
		stat := graphqlbackend.CreateFileInfo("CODEOWNERS", false)
		return graphqlbackend.NewVirtualFileResolver(stat, func(ctx context.Context) (string, error) {
			f, err := r.db.Codeowners().GetCodeownersForRepo(ctx, api.RepoID(src.ID))
			if err != nil {
				return "", err
			}
			return f.Contents, nil
		}, graphqlbackend.VirtualFileResolverOptions{
			URL: fmt.Sprintf("%s/-/own", r.repo.URL()),
		}), nil
	case codeowners.GitRulesetSource:
		// For committed, we can return a GitTreeEntry, as it implements File2.
		c := graphqlbackend.NewGitCommitResolver(r.db, r.gitserverClient, r.repo, src.Commit, nil)
		return c.File(ctx, &struct{ Path string }{Path: src.Path})
	default:
		return nil, errors.New("unknown ownership file source")
	}
}

func (r *codeownersFileEntryResolver) RuleLineMatch(_ context.Context) (int32, error) {
	return r.matchLineNumber, nil
}

func areOwnEndpointsAvailable(ctx context.Context) error {
	if !featureflag.FromContext(ctx).GetBoolOr("search-ownership", false) {
		return errors.New("own is not available yet")
	}
	return nil
}

func (r *ownResolver) AggregatedOwners(ctx context.Context,
	repo *graphqlbackend.RepositoryResolver,
	args graphqlbackend.AggregatedOwnersArgs,
) (graphqlbackend.AggregatedOwnershipConnectionResolver, error) {
	repoName := api.RepoName(repo.Name())
	repoID := repo.IDInt32()
	commitID := api.CommitID(args.Revision)
	ownService := r.ownService()
	rs, err := ownService.RulesetForRepo(ctx, repoName, repoID, commitID)
	if err != nil {
		return nil, err
	}
	fs, err := r.gitserver.ListFiles(ctx, nil, api.RepoName(repo.Name()), api.CommitID(args.Revision), regexp.MustCompile(""))
	type key struct {
		handle string
		email  string
	}
	owners := map[key]aggregatedOwnershipResolver{}
	for _, f := range fs {
		fmt.Println(f)
		if rule := rs.Match(f); rule != nil {
			for _, x := range rule.GetOwner() {
				ow := *x
				stats := owners[key{handle: ow.Handle, email: ow.Email}]
				if stats.totalFiles == 0 {
					fmt.Println(ow.Handle, ow.Email)
				}
				ro, err := ownService.ResolveOwnersWithType(ctx, []*codeownerspb.Owner{&ow})
				if err != nil {
					return nil, err
				}
				if len(ro) == 1 {
					stats.owner = ro[0]
				}
				stats.db = r.db
				stats.totalFiles++
				stats.exampleReason = &codeownersFileEntryResolver{
					db:              r.db,
					gitserverClient: r.gitserver,
					source:          rs.GetSource(),
					repo:            repo,
					matchLineNumber: rule.GetLineNumber(),
				}
				stats.order = ow.Handle + ow.Email
				owners[key{handle: ow.Handle, email: ow.Email}] = stats
			}
		}
	}
	var os []*aggregatedOwnershipResolver
	for _, v := range owners {
		w := v
		os = append(os, &w)
	}
	// highest total files at the beginning
	sort.Slice(os, func(i, j int) bool { return !(os[i].totalFiles < os[j].totalFiles) })
	return &aggregatedOwnershipConnectionResolver{owners: os, args: args}, err
}

type aggregatedOwnershipConnectionResolver struct {
	owners []*aggregatedOwnershipResolver
	args   graphqlbackend.AggregatedOwnersArgs
}

func (r *aggregatedOwnershipConnectionResolver) startIndex() int {
	for i := 0; r.args.After != nil && i < len(r.owners); i++ {
		if r.owners[i].order == *r.args.After {
			return i
		}
	}
	return 0
}

func (r *aggregatedOwnershipConnectionResolver) endIndex() int {
	idx := len(r.owners)
	if x := r.startIndex() + int(r.args.GetFirst()); x < idx {
		return x
	}
	return idx
}

func (r *aggregatedOwnershipConnectionResolver) TotalCount(ctx context.Context) int32 {
	return int32(len(r.owners))
}

func (r *aggregatedOwnershipConnectionResolver) PageInfo(ctx context.Context) *graphqlutil.PageInfo {
	start := r.startIndex()
	if len(r.owners) <= start+int(r.args.GetFirst()) {
		return graphqlutil.HasNextPage(false)
	}
	return graphqlutil.EncodeCursor(&r.owners[start+int(r.args.GetFirst())].order)
}

func (r *aggregatedOwnershipConnectionResolver) Nodes(ctx context.Context) []graphqlbackend.AggregatedOwnershipResolver {
	var rs []graphqlbackend.AggregatedOwnershipResolver
	for _, o := range r.owners[r.startIndex():r.endIndex()] {
		rs = append(rs, o)
	}
	return rs
}

type aggregatedOwnershipResolver struct {
	db            edb.EnterpriseDB
	order         string
	owner         codeowners.ResolvedOwner
	totalFiles    int32
	exampleReason *codeownersFileEntryResolver
}

func (r *aggregatedOwnershipResolver) Owner(ctx context.Context) (graphqlbackend.OwnerResolver, error) {
	return &ownerResolver{db: r.db, resolvedOwner: r.owner}, nil
}

func (r *aggregatedOwnershipResolver) TotalFiles(ctx context.Context) int32 { return r.totalFiles }

func (r *aggregatedOwnershipResolver) Reasons(ctx context.Context) ([]graphqlbackend.OwnershipReasonResolver, error) {
	return []graphqlbackend.OwnershipReasonResolver{r.exampleReason}, nil
}
