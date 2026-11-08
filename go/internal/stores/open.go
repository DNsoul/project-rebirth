package stores

import (
	"awgrotator/internal/gist"
	"awgrotator/internal/gitlab"
	"awgrotator/internal/proto"
)

// GitLabAccess says where the GitLab place is and how it is written to.
type GitLabAccess struct {
	Base    string
	Project int64
	Package string

	// Token may be empty: the place can then be read but not written, which is
	// all an app, or a tool run without a token, needs.
	Token string

	// Deploy says the token is a deploy token and not the operator's own.
	Deploy bool
}

// NewSet puts together the places: the secret gist on GitHub always, and the
// GitLab package when there is one.
func NewSet(trust proto.Trust, githubToken, gistID string, gl *GitLabAccess) *Set {
	s := &Set{
		Trust:  trust,
		Stores: []Store{Gist{Client: gist.New(githubToken), ID: gistID}},
	}
	if gl != nil {
		c := gitlab.New(gl.Base, gl.Token)
		c.Deploy = gl.Deploy
		s.Stores = append(s.Stores, GitLab{Client: c, Project: gl.Project, Package: gl.Package})
	}
	return s
}
