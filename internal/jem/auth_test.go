// Copyright 2015 Canonical Ltd.

package jem_test

import (
	"fmt"
	"net/http"

	"github.com/CanonicalLtd/blues-identity/idmclient"
	jujutesting "github.com/juju/testing"
	jc "github.com/juju/testing/checkers"
	gc "gopkg.in/check.v1"
	"gopkg.in/errgo.v1"
	"gopkg.in/macaroon-bakery.v1/bakery"
	"gopkg.in/macaroon-bakery.v1/httpbakery"

	"github.com/CanonicalLtd/jem/internal/idmtest"
	"github.com/CanonicalLtd/jem/internal/jem"
	"github.com/CanonicalLtd/jem/internal/mongodoc"
	"github.com/CanonicalLtd/jem/params"
)

type authSuite struct {
	jujutesting.IsolatedMgoSuite
	idmSrv *idmtest.Server
	pool   *jem.Pool
	jem    *jem.JEM
}

var _ = gc.Suite(&authSuite{})

func (s *authSuite) SetUpTest(c *gc.C) {
	s.IsolatedMgoSuite.SetUpTest(c)
	s.idmSrv = idmtest.NewServer()
	pool, err := jem.NewPool(
		jem.ServerParams{
			DB:               s.Session.DB("jem"),
			IdentityLocation: s.idmSrv.URL.String(),
			PublicKeyLocator: s.idmSrv,
			StateServerAdmin: "admin",
		},
		bakery.NewServiceParams{
			Location: "here",
			Locator:  s.idmSrv,
		},
		idmclient.New(idmclient.NewParams{
			BaseURL: s.idmSrv.URL.String(),
			Client:  s.idmSrv.Client("agent"),
		}),
	)
	c.Assert(err, gc.IsNil)
	s.pool = pool
	s.jem = s.pool.JEM()
}

func (s *authSuite) TearDownTest(c *gc.C) {
	s.jem.Close()
	s.IsolatedMgoSuite.TearDownTest(c)
}

func (s *authSuite) TestNewMacaroon(c *gc.C) {
	m, err := s.jem.NewMacaroon()
	c.Assert(err, gc.IsNil)
	c.Assert(m.Location(), gc.Equals, "here")
	c.Assert(m.Id(), gc.Not(gc.Equals), "")
	cavs := m.Caveats()
	c.Assert(cavs, gc.HasLen, 1)
	cav := cavs[0]
	c.Assert(cav.Location, gc.Equals, s.idmSrv.URL.String())
}

func (s *authSuite) TestAuthenticateNoMacaroon(c *gc.C) {
	req, err := http.NewRequest("GET", "/", nil)
	c.Assert(err, gc.IsNil)
	err = s.jem.Authenticate(req)
	c.Assert(err, gc.NotNil)
	berr, ok := err.(*httpbakery.Error)
	c.Assert(ok, gc.Equals, true, gc.Commentf("expected %T, got %T", berr, err))
	c.Assert(berr.Code, gc.Equals, httpbakery.ErrDischargeRequired)
	c.Assert(berr.Info, gc.NotNil)
	c.Assert(berr.Info.Macaroon, gc.NotNil)
	c.Assert(berr.Info.MacaroonPath, gc.Equals, "/")
}

func (s *authSuite) TestAuthenticate(c *gc.C) {
	req := s.newRequestForUser(c, "GET", "/", "bob")
	err := s.jem.Authenticate(req)
	c.Assert(err, gc.IsNil)
	c.Assert(s.jem.Auth.Username, gc.Equals, "bob")
}

func (s *authSuite) TestCheckIsAdmin(c *gc.C) {
	req := s.newRequestForUser(c, "GET", "/", "admin")
	err := s.jem.Authenticate(req)
	c.Assert(err, gc.IsNil)
	c.Assert(s.jem.CheckIsAdmin(), gc.IsNil)
	req = s.newRequestForUser(c, "GET", "/", "bob")
	err = s.jem.Authenticate(req)
	c.Assert(err, gc.IsNil)
	c.Assert(s.jem.CheckIsAdmin(), gc.ErrorMatches, string(params.ErrUnauthorized))
}

func (s *authSuite) TestCheckIsUser(c *gc.C) {
	req := s.newRequestForUser(c, "GET", "/", "bob")
	err := s.jem.Authenticate(req)
	c.Assert(err, gc.IsNil)
	c.Assert(s.jem.CheckIsUser("fred"), gc.ErrorMatches, string(params.ErrUnauthorized))
	c.Assert(s.jem.CheckIsUser("bob"), gc.IsNil)
}

func (s *authSuite) TestCheckACL(c *gc.C) {
	c.Assert(s.jem.CheckACL([]string{"admin"}), gc.ErrorMatches, `cannot check permissions: cannot fetch groups: missing value for path parameter "username"`)
	req := s.newRequestForUser(c, "GET", "/", "bob", "bob-group")
	err := s.jem.Authenticate(req)
	c.Assert(err, gc.IsNil)
	c.Assert(s.jem.CheckACL([]string{}), gc.ErrorMatches, string(params.ErrUnauthorized))
	c.Assert(s.jem.CheckACL([]string{"bob"}), gc.IsNil)
	c.Assert(s.jem.CheckACL([]string{"bob-group"}), gc.IsNil)
}

var canReadTests = []struct {
	owner   string
	readers []string
	allowed bool
}{{
	owner:   "bob",
	allowed: true,
}, {
	owner: "fred",
}, {
	owner:   "fred",
	readers: []string{"bob"},
	allowed: true,
}, {
	owner:   "fred",
	readers: []string{"bob-group"},
	allowed: true,
}, {
	owner:   "bob-group",
	allowed: true,
}, {
	owner:   "fred",
	readers: []string{"everyone"},
	allowed: true,
}, {
	owner:   "fred",
	readers: []string{"harry", "john"},
}, {
	owner:   "fred",
	readers: []string{"harry", "bob-group"},
	allowed: true,
}}

func (s *authSuite) TestCheckCanRead(c *gc.C) {
	req := s.newRequestForUser(c, "GET", "/", "bob", "bob-group")
	err := s.jem.Authenticate(req)
	c.Assert(err, gc.IsNil)
	for i, test := range canReadTests {
		c.Logf("%d. %q %#v", i, test.owner, test.readers)
		err := s.jem.CheckCanRead(testEntity{
			owner:   test.owner,
			readers: test.readers,
		})
		if test.allowed {
			c.Assert(err, gc.IsNil)
			continue
		}
		c.Assert(err, gc.ErrorMatches, string(params.ErrUnauthorized))
	}
}

var checkReadACLTests = []struct {
	about            string
	owner            string
	acl              []string
	user             string
	groups           []string
	skipCreateEntity bool
	expectError      string
	expectCause      error
}{{
	about: "user is owner",
	owner: "bob",
	user:  "bob",
}, {
	about:  "owner is user group",
	owner:  "bobgroup",
	user:   "bob",
	groups: []string{"bobgroup"},
}, {
	about: "acl contains user",
	owner: "fred",
	acl:   []string{"bob"},
	user:  "bob",
}, {
	about:  "acl contains user's group",
	owner:  "fred",
	acl:    []string{"bobgroup"},
	user:   "bob",
	groups: []string{"bobgroup"},
}, {
	about:       "user not in acl",
	owner:       "fred",
	acl:         []string{"fredgroup"},
	user:        "bob",
	expectError: "unauthorized",
	expectCause: params.ErrUnauthorized,
}, {
	about:            "no entity and not owner",
	owner:            "fred",
	user:             "bob",
	skipCreateEntity: true,
	expectError:      "unauthorized",
	expectCause:      params.ErrUnauthorized,
}}

func (s *authSuite) TestCheckReadACL(c *gc.C) {
	for i, test := range checkReadACLTests {
		c.Logf("%d. %s", i, test.about)
		func() {
			jem := s.pool.JEM()
			defer jem.Close()
			req := s.newRequestForUser(c, "GET", "", test.user, test.groups...)
			err := jem.Authenticate(req)
			c.Assert(err, gc.IsNil)
			entity := params.EntityPath{
				User: params.User(test.owner),
				Name: params.Name(fmt.Sprintf("test%d", i)),
			}
			if !test.skipCreateEntity {
				err := jem.AddEnvironment(&mongodoc.Environment{
					Path: entity,
					ACL: params.ACL{
						Read: test.acl,
					},
				})
				c.Assert(err, gc.IsNil)
			}
			err = jem.CheckReadACL(jem.DB.Environments(), entity)
			if test.expectError != "" {
				c.Assert(err, gc.ErrorMatches, test.expectError)
				if test.expectCause != nil {
					c.Assert(errgo.Cause(err), gc.Equals, test.expectCause)
				} else {
					c.Assert(errgo.Cause(err), gc.Equals, err)
				}
			} else {
				c.Assert(err, gc.IsNil)
			}
		}()
	}
}

func (s *authSuite) TestCheckGetACL(c *gc.C) {
	env := &mongodoc.Environment{
		Path: params.EntityPath{
			User: params.User("bob"),
			Name: "env",
		},
		ACL: params.ACL{
			Read: []string{"fred", "jim"},
		},
	}
	err := s.jem.AddEnvironment(env)
	c.Assert(err, gc.IsNil)
	acl, err := s.jem.GetACL(s.jem.DB.Environments(), env.Path)
	c.Assert(err, gc.IsNil)
	c.Assert(acl, jc.DeepEquals, env.ACL)
}

func (s *authSuite) TestCheckGetACLNotFound(c *gc.C) {
	env := &mongodoc.Environment{
		Path: params.EntityPath{
			User: params.User("bob"),
			Name: "env",
		},
	}
	acl, err := s.jem.GetACL(s.jem.DB.Environments(), env.Path)
	c.Assert(err, gc.ErrorMatches, "not found")
	c.Assert(errgo.Cause(err), gc.Equals, params.ErrNotFound)
	c.Assert(acl, jc.DeepEquals, env.ACL)
}

func (s *authSuite) TestCanReadIter(c *gc.C) {
	testEnvs := []mongodoc.Environment{{
		Path: params.EntityPath{
			User: params.User("bob"),
			Name: "env1",
		},
	}, {
		Path: params.EntityPath{
			User: params.User("fred"),
			Name: "env2",
		},
	}, {
		Path: params.EntityPath{
			User: params.User("fred"),
			Name: "env3",
		},
		ACL: params.ACL{
			Read: []string{"bob"},
		},
	}}
	for i := range testEnvs {
		err := s.jem.AddEnvironment(&testEnvs[i])
		c.Assert(err, gc.IsNil)
	}
	req := s.newRequestForUser(c, "GET", "/", "bob", "bob-group")
	err := s.jem.Authenticate(req)
	c.Assert(err, gc.IsNil)
	it := s.jem.DB.Environments().Find(nil).Sort("_id").Iter()
	crit := s.jem.CanReadIter(it)
	var envs []mongodoc.Environment
	var env mongodoc.Environment
	for crit.Next(&env) {
		envs = append(envs, env)
	}
	c.Assert(crit.Err(), gc.IsNil)
	c.Assert(envs, jc.DeepEquals, []mongodoc.Environment{
		testEnvs[0],
		testEnvs[2],
	})
}

// newRequestForUser builds a new *http.Request for method at path which
// includes a macaroon authenticating username who will be placed in the
// specified groups.
func (s *authSuite) newRequestForUser(c *gc.C, method, path, username string, groups ...string) *http.Request {
	s.idmSrv.AddUser(username, groups...)
	s.idmSrv.SetDefaultUser(username)
	cl := s.idmSrv.Client(username)
	m, err := s.jem.NewMacaroon()
	c.Assert(err, gc.IsNil)
	ms, err := cl.DischargeAll(m)
	c.Assert(err, gc.IsNil)
	cookie, err := httpbakery.NewCookie(ms)
	c.Assert(err, gc.IsNil)
	req, err := http.NewRequest(method, path, nil)
	c.Assert(err, gc.IsNil)
	req.AddCookie(cookie)
	return req
}

type testEntity struct {
	owner   string
	readers []string
}

func (e testEntity) Owner() params.User {
	return params.User(e.owner)
}

func (e testEntity) GetACL() params.ACL {
	return params.ACL{
		Read: e.readers,
	}
}

var _ jem.ACLEntity = testEntity{}