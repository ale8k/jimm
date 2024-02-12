// Copyright 2023 Canonical Ltd.

package jimm_test

import (
	"context"
	"database/sql"
	"sort"
	"testing"
	"time"

	petname "github.com/dustinkirkland/golang-petname"
	qt "github.com/frankban/quicktest"
	"github.com/google/uuid"
	"github.com/juju/juju/core/crossmodel"
	jujuparams "github.com/juju/juju/rpc/params"
	"github.com/juju/names/v4"

	"github.com/canonical/jimm/internal/constants"
	"github.com/canonical/jimm/internal/db"
	"github.com/canonical/jimm/internal/dbmodel"
	"github.com/canonical/jimm/internal/errors"
	"github.com/canonical/jimm/internal/jimm"
	"github.com/canonical/jimm/internal/jimmjwx"
	"github.com/canonical/jimm/internal/jimmtest"
	"github.com/canonical/jimm/internal/openfga"
	ofganames "github.com/canonical/jimm/internal/openfga/names"
	jimmnames "github.com/canonical/jimm/pkg/names"
	"github.com/canonical/ofga"
)

// testAuthenticator is an authenticator implementation intended
// for testing the token generator.
type testAuthenticator struct {
	username string
	err      error
}

// Authenticate implements the Authenticate method of the Authenticator interface.
func (ta *testAuthenticator) Authenticate(ctx context.Context, req *jujuparams.LoginRequest) (*openfga.User, error) {
	if ta.err != nil {
		return nil, ta.err
	}
	return &openfga.User{
		Identity: &dbmodel.Identity{
			Name: ta.username,
		},
	}, nil
}

// testDatabase is a database implementation intended for testing the token generator.
type testDatabase struct {
	ctl dbmodel.Controller
	err error
}

// GetController implements the GetController method of the JWTGeneratorDatabase interface.
func (tdb *testDatabase) GetController(ctx context.Context, controller *dbmodel.Controller) error {
	if tdb.err != nil {
		return tdb.err
	}
	*controller = tdb.ctl
	return nil
}

// testAccessChecker is an access checker implementation intended for testing the
// token generator.
type testAccessChecker struct {
	controllerAccess         map[string]string
	controllerAccessCheckErr error
	modelAccess              map[string]string
	modelAccessCheckErr      error
	cloudAccess              map[string]string
	cloudAccessCheckErr      error
	permissions              map[string]string
	permissionCheckErr       error
}

// GetUserModelAccess implements the GetUserModelAccess method of the JWTGeneratorAccessChecker interface.
func (tac *testAccessChecker) GetUserModelAccess(ctx context.Context, user *openfga.User, mt names.ModelTag) (string, error) {
	if tac.modelAccessCheckErr != nil {
		return "", tac.modelAccessCheckErr
	}
	return tac.modelAccess[mt.String()], nil
}

// GetUserControllerAccess implements the GetUserControllerAccess method of the JWTGeneratorAccessChecker interface.
func (tac *testAccessChecker) GetUserControllerAccess(ctx context.Context, user *openfga.User, ct names.ControllerTag) (string, error) {
	if tac.controllerAccessCheckErr != nil {
		return "", tac.controllerAccessCheckErr
	}
	return tac.controllerAccess[ct.String()], nil
}

// GetUserCloudAccess implements the GetUserCloudAccess method of the JWTGeneratorAccessChecker interface.
func (tac *testAccessChecker) GetUserCloudAccess(ctx context.Context, user *openfga.User, ct names.CloudTag) (string, error) {
	if tac.cloudAccessCheckErr != nil {
		return "", tac.cloudAccessCheckErr
	}
	return tac.cloudAccess[ct.String()], nil
}

// CheckPermission implements the CheckPermission methods of the JWTGeneratorAccessChecker interface.
func (tac *testAccessChecker) CheckPermission(ctx context.Context, user *openfga.User, accessMap map[string]string, permissions map[string]interface{}) (map[string]string, error) {
	if tac.permissionCheckErr != nil {
		return nil, tac.permissionCheckErr
	}
	access := make(map[string]string)
	for k, v := range accessMap {
		access[k] = v
	}
	for k, v := range tac.permissions {
		access[k] = v
	}
	return access, nil
}

// testJWTService is a jwt service implementation intended for testing the token generator.
type testJWTService struct {
	newJWTError error

	params jimmjwx.JWTParams
}

// NewJWT implements the NewJWT methods of the JWTService interface.
func (t *testJWTService) NewJWT(ctx context.Context, params jimmjwx.JWTParams) ([]byte, error) {
	if t.newJWTError != nil {
		return nil, t.newJWTError
	}
	t.params = params
	return []byte("test jwt"), nil
}

func TestAuditLogAccess(t *testing.T) {
	c := qt.New(t)

	ofgaClient, _, _, err := jimmtest.SetupTestOFGAClient(c.Name())
	c.Assert(err, qt.IsNil)

	now := time.Now().UTC().Round(time.Millisecond)
	j := &jimm.JIMM{
		UUID: uuid.NewString(),
		Database: db.Database{
			DB: jimmtest.PostgresDB(c, func() time.Time { return now }),
		},
		OpenFGAClient: ofgaClient,
	}
	ctx := context.Background()

	err = j.Database.Migrate(ctx, false)
	c.Assert(err, qt.IsNil)

	adminUser := openfga.NewUser(&dbmodel.Identity{Name: "alice"}, j.OpenFGAClient)
	err = adminUser.SetControllerAccess(ctx, j.ResourceTag(), ofganames.AdministratorRelation)
	c.Assert(err, qt.IsNil)

	user := openfga.NewUser(&dbmodel.Identity{Name: "bob"}, j.OpenFGAClient)

	// admin user can grant other users audit log access.
	err = j.GrantAuditLogAccess(ctx, adminUser, user.ResourceTag())
	c.Assert(err, qt.IsNil)

	access := user.GetAuditLogViewerAccess(ctx, j.ResourceTag())
	c.Assert(access, qt.Equals, ofganames.AuditLogViewerRelation)

	// re-granting access does not result in error.
	err = j.GrantAuditLogAccess(ctx, adminUser, user.ResourceTag())
	c.Assert(err, qt.IsNil)

	// admin user can revoke other users audit log access.
	err = j.RevokeAuditLogAccess(ctx, adminUser, user.ResourceTag())
	c.Assert(err, qt.IsNil)

	access = user.GetAuditLogViewerAccess(ctx, j.ResourceTag())
	c.Assert(access, qt.Equals, ofganames.NoRelation)

	// re-revoking access does not result in error.
	err = j.RevokeAuditLogAccess(ctx, adminUser, user.ResourceTag())
	c.Assert(err, qt.IsNil)

	// non-admin user cannot grant audit log access
	err = j.GrantAuditLogAccess(ctx, user, adminUser.ResourceTag())
	c.Assert(err, qt.ErrorMatches, "unauthorized")

	// non-admin user cannot revoke audit log access
	err = j.RevokeAuditLogAccess(ctx, user, adminUser.ResourceTag())
	c.Assert(err, qt.ErrorMatches, "unauthorized")
}

func TestJWTGeneratorMakeLoginToken(t *testing.T) {
	c := qt.New(t)

	ct := names.NewControllerTag(uuid.New().String())
	mt := names.NewModelTag(uuid.New().String())

	tests := []struct {
		about             string
		authenticator     *testAuthenticator
		database          *testDatabase
		accessChecker     *testAccessChecker
		jwtService        *testJWTService
		expectedError     string
		expectedJWTParams jimmjwx.JWTParams
	}{{
		about: "initial login, all is well",
		authenticator: &testAuthenticator{
			username: "eve@external",
		},
		database: &testDatabase{
			ctl: dbmodel.Controller{
				CloudRegions: []dbmodel.CloudRegionControllerPriority{{
					CloudRegion: dbmodel.CloudRegion{
						Cloud: dbmodel.Cloud{
							Name: "test-cloud",
						},
					},
				}},
			},
		},
		accessChecker: &testAccessChecker{
			modelAccess: map[string]string{
				mt.String(): "admin",
			},
			controllerAccess: map[string]string{
				ct.String(): "superuser",
			},
			cloudAccess: map[string]string{
				names.NewCloudTag("test-cloud").String(): "add-model",
			},
		},
		jwtService: &testJWTService{},
		expectedJWTParams: jimmjwx.JWTParams{
			Controller: ct.Id(),
			User:       names.NewUserTag("eve@external").String(),
			Access: map[string]string{
				ct.String():                              "superuser",
				mt.String():                              "admin",
				names.NewCloudTag("test-cloud").String(): "add-model",
			},
		},
	}, {
		about: "authorization fails",
		authenticator: &testAuthenticator{
			username: "eve@external",
			err:      errors.E("a test error"),
		},
		expectedError: "a test error",
	}, {
		about: "model access check fails",
		authenticator: &testAuthenticator{
			username: "eve@external",
		},
		accessChecker: &testAccessChecker{
			modelAccessCheckErr: errors.E("a test error"),
		},
		jwtService:    &testJWTService{},
		expectedError: "a test error",
	}, {
		about: "controller access check fails",
		authenticator: &testAuthenticator{
			username: "eve@external",
		},
		accessChecker: &testAccessChecker{
			modelAccess: map[string]string{
				mt.String(): "admin",
			},
			controllerAccessCheckErr: errors.E("a test error"),
		},
		expectedError: "a test error",
	}, {
		about: "get controller from db fails",
		authenticator: &testAuthenticator{
			username: "eve@external",
		},
		database: &testDatabase{
			err: errors.E("a test error"),
		},
		accessChecker: &testAccessChecker{
			modelAccess: map[string]string{
				mt.String(): "admin",
			},
			controllerAccess: map[string]string{
				ct.String(): "superuser",
			},
		},
		expectedError: "failed to fetch controller",
	}, {
		about: "cloud access check fails",
		authenticator: &testAuthenticator{
			username: "eve@external",
		},
		database: &testDatabase{
			ctl: dbmodel.Controller{
				CloudRegions: []dbmodel.CloudRegionControllerPriority{{
					CloudRegion: dbmodel.CloudRegion{
						Cloud: dbmodel.Cloud{
							Name: "test-cloud",
						},
					},
				}},
			},
		},
		accessChecker: &testAccessChecker{
			modelAccess: map[string]string{
				mt.String(): "admin",
			},
			controllerAccess: map[string]string{
				ct.String(): "superuser",
			},
			cloudAccessCheckErr: errors.E("a test error"),
		},
		expectedError: "failed to check user's cloud access",
	}, {
		about: "jwt service errors out",
		authenticator: &testAuthenticator{
			username: "eve@external",
		},
		database: &testDatabase{
			ctl: dbmodel.Controller{
				CloudRegions: []dbmodel.CloudRegionControllerPriority{{
					CloudRegion: dbmodel.CloudRegion{
						Cloud: dbmodel.Cloud{
							Name: "test-cloud",
						},
					},
				}},
			},
		},
		accessChecker: &testAccessChecker{
			modelAccess: map[string]string{
				mt.String(): "admin",
			},
			controllerAccess: map[string]string{
				ct.String(): "superuser",
			},
			cloudAccess: map[string]string{
				names.NewCloudTag("test-cloud").String(): "add-model",
			},
		},
		jwtService: &testJWTService{
			newJWTError: errors.E("a test error"),
		},
		expectedError: "a test error",
	}}

	for _, test := range tests {
		generator := jimm.NewJWTGenerator(test.authenticator, test.database, test.accessChecker, test.jwtService)
		generator.SetTags(mt, ct)

		_, err := generator.MakeLoginToken(context.Background(), &jujuparams.LoginRequest{})
		if test.expectedError != "" {
			c.Assert(err, qt.ErrorMatches, test.expectedError)
		} else {
			c.Assert(err, qt.IsNil)
			c.Assert(test.jwtService.params, qt.DeepEquals, test.expectedJWTParams)
		}
	}
}

func TestJWTGeneratorMakeToken(t *testing.T) {
	c := qt.New(t)

	ct := names.NewControllerTag(uuid.New().String())
	mt := names.NewModelTag(uuid.New().String())

	tests := []struct {
		about                 string
		checkPermissions      map[string]string
		checkPermissionsError error
		jwtService            *testJWTService
		expectedError         string
		permissions           map[string]interface{}
		expectedJWTParams     jimmjwx.JWTParams
	}{{
		about:      "all is well",
		jwtService: &testJWTService{},
		expectedJWTParams: jimmjwx.JWTParams{
			Controller: ct.Id(),
			User:       names.NewUserTag("eve@external").String(),
			Access: map[string]string{
				ct.String():                              "superuser",
				mt.String():                              "admin",
				names.NewCloudTag("test-cloud").String(): "add-model",
			},
		},
	}, {
		about:      "check permission fails",
		jwtService: &testJWTService{},
		permissions: map[string]interface{}{
			"entity1": "access_level1",
		},
		checkPermissionsError: errors.E("a test error"),
		expectedError:         "a test error",
	}, {
		about:      "additional permissions need checking",
		jwtService: &testJWTService{},
		permissions: map[string]interface{}{
			"entity1": "access_level1",
		},
		checkPermissions: map[string]string{
			"entity1": "access_level1",
		},
		expectedJWTParams: jimmjwx.JWTParams{
			Controller: ct.Id(),
			User:       names.NewUserTag("eve@external").String(),
			Access: map[string]string{
				ct.String():                              "superuser",
				mt.String():                              "admin",
				names.NewCloudTag("test-cloud").String(): "add-model",
				"entity1":                                "access_level1",
			},
		},
	}}

	for _, test := range tests {
		generator := jimm.NewJWTGenerator(
			&testAuthenticator{
				username: "eve@external",
			},
			&testDatabase{
				ctl: dbmodel.Controller{
					CloudRegions: []dbmodel.CloudRegionControllerPriority{{
						CloudRegion: dbmodel.CloudRegion{
							Cloud: dbmodel.Cloud{
								Name: "test-cloud",
							},
						},
					}},
				},
			},
			&testAccessChecker{
				modelAccess: map[string]string{
					mt.String(): "admin",
				},
				controllerAccess: map[string]string{
					ct.String(): "superuser",
				},
				cloudAccess: map[string]string{
					names.NewCloudTag("test-cloud").String(): "add-model",
				},
				permissions:        test.checkPermissions,
				permissionCheckErr: test.checkPermissionsError,
			},
			test.jwtService,
		)
		generator.SetTags(mt, ct)

		_, err := generator.MakeLoginToken(context.Background(), &jujuparams.LoginRequest{})
		c.Assert(err, qt.IsNil)

		_, err = generator.MakeToken(context.Background(), test.permissions)
		if test.expectedError != "" {
			c.Assert(err, qt.ErrorMatches, test.expectedError)
		} else {
			c.Assert(err, qt.IsNil)
			c.Assert(test.jwtService.params, qt.DeepEquals, test.expectedJWTParams)
		}
	}
}

func TestParseTag(t *testing.T) {
	c := qt.New(t)
	ctx := context.Background()

	ofgaClient, _, _, err := jimmtest.SetupTestOFGAClient(c.Name())
	c.Assert(err, qt.IsNil)

	now := time.Now().UTC().Round(time.Millisecond)
	j := &jimm.JIMM{
		UUID: uuid.NewString(),
		Database: db.Database{
			DB: jimmtest.PostgresDB(c, func() time.Time { return now }),
		},
		OpenFGAClient: ofgaClient,
	}

	err = j.Database.Migrate(ctx, false)
	c.Assert(err, qt.IsNil)

	user, _, controller, model, _, _, _ := createTestControllerEnvironment(ctx, c, j.Database)

	jimmTag := "model-" + controller.Name + ":" + user.Username + "/" + model.Name + "#administrator"

	// JIMM tag syntax for models
	tag, err := j.ParseTag(ctx, jimmTag)
	c.Assert(err, qt.IsNil)
	c.Assert(tag.Kind.String(), qt.Equals, names.ModelTagKind)
	c.Assert(tag.ID, qt.Equals, model.UUID.String)
	c.Assert(tag.Relation.String(), qt.Equals, "administrator")

	jujuTag := "model-" + model.UUID.String + "#administrator"

	// Juju tag syntax for models
	tag, err = j.ParseTag(ctx, jujuTag)
	c.Assert(err, qt.IsNil)
	c.Assert(tag.ID, qt.Equals, model.UUID.String)
	c.Assert(tag.Kind.String(), qt.Equals, names.ModelTagKind)
	c.Assert(tag.Relation.String(), qt.Equals, "administrator")
}

func TestResolveJIMM(t *testing.T) {
	c := qt.New(t)
	ctx := context.Background()

	ofgaClient, _, _, err := jimmtest.SetupTestOFGAClient(c.Name())
	c.Assert(err, qt.IsNil)

	now := time.Now().UTC().Round(time.Millisecond)
	j := &jimm.JIMM{
		UUID: uuid.NewString(),
		Database: db.Database{
			DB: jimmtest.PostgresDB(c, func() time.Time { return now }),
		},
		OpenFGAClient: ofgaClient,
	}

	err = j.Database.Migrate(ctx, false)
	c.Assert(err, qt.IsNil)

	jimmTag := "controller-jimm"

	jujuTag, err := jimm.ResolveTag(j.UUID, &j.Database, jimmTag)
	c.Assert(err, qt.IsNil)
	c.Assert(jujuTag, qt.DeepEquals, ofganames.ConvertTag(names.NewControllerTag(j.UUID)))
}

func TestResolveTupleObjectMapsApplicationOffersUUIDs(t *testing.T) {
	c := qt.New(t)
	ctx := context.Background()

	ofgaClient, _, _, err := jimmtest.SetupTestOFGAClient(c.Name())
	c.Assert(err, qt.IsNil)

	now := time.Now().UTC().Round(time.Millisecond)
	j := &jimm.JIMM{
		UUID: uuid.NewString(),
		Database: db.Database{
			DB: jimmtest.PostgresDB(c, func() time.Time { return now }),
		},
		OpenFGAClient: ofgaClient,
	}

	err = j.Database.Migrate(ctx, false)
	c.Assert(err, qt.IsNil)

	user, _, controller, model, offer, _, _ := createTestControllerEnvironment(ctx, c, j.Database)

	jimmTag := "applicationoffer-" + controller.Name + ":" + user.Username + "/" + model.Name + "." + offer.Name + "#administrator"

	jujuTag, err := jimm.ResolveTag(j.UUID, &j.Database, jimmTag)
	c.Assert(err, qt.IsNil)
	c.Assert(jujuTag, qt.DeepEquals, ofganames.ConvertTagWithRelation(names.NewApplicationOfferTag(offer.UUID), ofganames.AdministratorRelation))
}

func TestResolveTupleObjectMapsModelUUIDs(t *testing.T) {
	c := qt.New(t)
	ctx := context.Background()

	ofgaClient, _, _, err := jimmtest.SetupTestOFGAClient(c.Name())
	c.Assert(err, qt.IsNil)

	now := time.Now().UTC().Round(time.Millisecond)
	j := &jimm.JIMM{
		UUID: uuid.NewString(),
		Database: db.Database{
			DB: jimmtest.PostgresDB(c, func() time.Time { return now }),
		},
		OpenFGAClient: ofgaClient,
	}

	err = j.Database.Migrate(ctx, false)
	c.Assert(err, qt.IsNil)

	user, _, controller, model, _, _, _ := createTestControllerEnvironment(ctx, c, j.Database)

	jimmTag := "model-" + controller.Name + ":" + user.Username + "/" + model.Name + "#administrator"

	tag, err := jimm.ResolveTag(j.UUID, &j.Database, jimmTag)
	c.Assert(err, qt.IsNil)
	c.Assert(tag, qt.DeepEquals, ofganames.ConvertTagWithRelation(names.NewModelTag(model.UUID.String), ofganames.AdministratorRelation))
}

func TestResolveTupleObjectMapsControllerUUIDs(t *testing.T) {
	c := qt.New(t)
	ctx := context.Background()

	ofgaClient, _, _, err := jimmtest.SetupTestOFGAClient(c.Name())
	c.Assert(err, qt.IsNil)

	now := time.Now().UTC().Round(time.Millisecond)
	j := &jimm.JIMM{
		UUID: uuid.NewString(),
		Database: db.Database{
			DB: jimmtest.PostgresDB(c, func() time.Time { return now }),
		},
		OpenFGAClient: ofgaClient,
	}

	err = j.Database.Migrate(ctx, false)
	c.Assert(err, qt.IsNil)

	cloud := dbmodel.Cloud{
		Name: "test-cloud",
	}
	err = j.Database.AddCloud(context.Background(), &cloud)
	c.Assert(err, qt.IsNil)

	uuid, _ := uuid.NewRandom()
	controller := dbmodel.Controller{
		Name:      "mycontroller",
		UUID:      uuid.String(),
		CloudName: "test-cloud",
	}
	err = j.Database.AddController(ctx, &controller)
	c.Assert(err, qt.IsNil)

	tag, err := jimm.ResolveTag(j.UUID, &j.Database, "controller-mycontroller#administrator")
	c.Assert(err, qt.IsNil)
	c.Assert(tag, qt.DeepEquals, ofganames.ConvertTagWithRelation(names.NewControllerTag(uuid.String()), ofganames.AdministratorRelation))
}

func TestResolveTupleObjectMapsGroups(t *testing.T) {
	c := qt.New(t)
	ctx := context.Background()

	ofgaClient, _, _, err := jimmtest.SetupTestOFGAClient(c.Name())
	c.Assert(err, qt.IsNil)

	now := time.Now().UTC().Round(time.Millisecond)
	j := &jimm.JIMM{
		UUID: uuid.NewString(),
		Database: db.Database{
			DB: jimmtest.PostgresDB(c, func() time.Time { return now }),
		},
		OpenFGAClient: ofgaClient,
	}

	err = j.Database.Migrate(ctx, false)
	c.Assert(err, qt.IsNil)

	err = j.Database.AddGroup(ctx, "myhandsomegroupofdigletts")
	c.Assert(err, qt.IsNil)
	group := &dbmodel.GroupEntry{
		Name: "myhandsomegroupofdigletts",
	}
	err = j.Database.GetGroup(ctx, group)
	c.Assert(err, qt.IsNil)
	tag, err := jimm.ResolveTag(j.UUID, &j.Database, "group-"+group.Name+"#member")
	c.Assert(err, qt.IsNil)
	c.Assert(tag, qt.DeepEquals, ofganames.ConvertTagWithRelation(jimmnames.NewGroupTag("1"), ofganames.MemberRelation))
}

func TestResolveTagObjectMapsUsers(t *testing.T) {
	c := qt.New(t)
	ctx := context.Background()

	ofgaClient, _, _, err := jimmtest.SetupTestOFGAClient(c.Name())
	c.Assert(err, qt.IsNil)

	now := time.Now().UTC().Round(time.Millisecond)
	j := &jimm.JIMM{
		UUID: uuid.NewString(),
		Database: db.Database{
			DB: jimmtest.PostgresDB(c, func() time.Time { return now }),
		},
		OpenFGAClient: ofgaClient,
	}

	err = j.Database.Migrate(ctx, false)
	c.Assert(err, qt.IsNil)

	tag, err := jimm.ResolveTag(j.UUID, &j.Database, "user-alex@externally-werly#member")
	c.Assert(err, qt.IsNil)
	c.Assert(tag, qt.DeepEquals, ofganames.ConvertTagWithRelation(names.NewUserTag("alex@externally-werly"), ofganames.MemberRelation))
}

func TestResolveTupleObjectHandlesErrors(t *testing.T) {
	c := qt.New(t)
	ctx := context.Background()

	ofgaClient, _, _, err := jimmtest.SetupTestOFGAClient(c.Name())
	c.Assert(err, qt.IsNil)

	now := time.Now().UTC().Round(time.Millisecond)
	j := &jimm.JIMM{
		UUID: uuid.NewString(),
		Database: db.Database{
			DB: jimmtest.PostgresDB(c, func() time.Time { return now }),
		},
		OpenFGAClient: ofgaClient,
	}

	err = j.Database.Migrate(ctx, false)
	c.Assert(err, qt.IsNil)

	_, _, controller, model, offer, _, _ := createTestControllerEnvironment(ctx, c, j.Database)

	type test struct {
		input string
		want  string
	}

	tests := []test{
		// Resolves bad tuple objects in general
		{
			input: "unknowntag-blabla",
			want:  "failed to map tag unknowntag",
		},
		// Resolves bad groups where they do not exist
		{
			input: "group-myspecialpokemon-his-name-is-youguessedit-diglett",
			want:  "group not found",
		},
		// Resolves bad controllers where they do not exist
		{
			input: "controller-mycontroller-that-does-not-exist",
			want:  "controller not found",
		},
		// Resolves bad models where the user cannot be obtained from the JIMM tag
		{
			input: "model-mycontroller-that-does-not-exist/mymodel",
			want:  "model not found",
		},
		// Resolves bad models where it cannot be found on the specified controller
		{
			input: "model-" + controller.Name + ":alex/",
			want:  "model not found",
		},
		// Resolves bad applicationoffers where it cannot be found on the specified controller/model combo
		{
			input: "applicationoffer-" + controller.Name + ":alex/" + model.Name + "." + offer.Name + "fluff",
			want:  "application offer not found",
		},
	}
	for _, tc := range tests {
		_, err := jimm.ResolveTag(j.UUID, &j.Database, tc.input)
		c.Assert(err, qt.ErrorMatches, tc.want)
	}
}

// createTestControllerEnvironment is a utility function creating the necessary components of adding a:
//   - user
//   - user group
//   - controller
//   - model
//   - application offer
//   - cloud
//   - cloud credential
//
// Into the test database, returning the dbmodels to be utilised for values within tests.
//
// It returns all of the latter, but in addition to those, also:
//   - an api client to make calls to an httptest instance of the server
//   - a closure containing a function to close the connection
//
// TODO(ale8k): Make this an implicit thing on the JIMM suite per test & refactor the current state.
// and make the suite argument an interface of the required calls we use here.
func createTestControllerEnvironment(ctx context.Context, c *qt.C, db db.Database) (
	dbmodel.User,
	dbmodel.GroupEntry,
	dbmodel.Controller,
	dbmodel.Model,
	dbmodel.ApplicationOffer,
	dbmodel.Cloud,
	dbmodel.CloudCredential) {

	err := db.AddGroup(ctx, "test-group")
	c.Assert(err, qt.IsNil)
	group := dbmodel.GroupEntry{Name: "test-group"}
	err = db.GetGroup(ctx, &group)
	c.Assert(err, qt.IsNil)

	u := dbmodel.User{
		Username: petname.Generate(2, "-") + "@external",
	}
	c.Assert(db.DB.Create(&u).Error, qt.IsNil)

	cloud := dbmodel.Cloud{
		Name: petname.Generate(2, "-"),
		Type: "aws",
		Regions: []dbmodel.CloudRegion{{
			Name: petname.Generate(2, "-"),
		}},
	}
	c.Assert(db.DB.Create(&cloud).Error, qt.IsNil)
	id, _ := uuid.NewRandom()
	controller := dbmodel.Controller{
		Name:        petname.Generate(2, "-"),
		UUID:        id.String(),
		CloudName:   cloud.Name,
		CloudRegion: cloud.Regions[0].Name,
		CloudRegions: []dbmodel.CloudRegionControllerPriority{{
			Priority:      0,
			CloudRegionID: cloud.Regions[0].ID,
		}},
	}
	err = db.AddController(ctx, &controller)
	c.Assert(err, qt.IsNil)

	cred := dbmodel.CloudCredential{
		Name:          petname.Generate(2, "-"),
		CloudName:     cloud.Name,
		OwnerUsername: u.Username,
		AuthType:      "empty",
	}
	err = db.SetCloudCredential(ctx, &cred)
	c.Assert(err, qt.IsNil)

	model := dbmodel.Model{
		Name: petname.Generate(2, "-"),
		UUID: sql.NullString{
			String: id.String(),
			Valid:  true,
		},
		OwnerUsername:     u.Username,
		ControllerID:      controller.ID,
		CloudRegionID:     cloud.Regions[0].ID,
		CloudCredentialID: cred.ID,
		Life:              constants.ALIVE.String(),
		Status: dbmodel.Status{
			Status: "available",
			Since: sql.NullTime{
				Time:  time.Now().UTC().Truncate(time.Millisecond),
				Valid: true,
			},
		},
	}

	err = db.AddModel(ctx, &model)
	c.Assert(err, qt.IsNil)

	offerName := petname.Generate(2, "-")
	offerURL, err := crossmodel.ParseOfferURL(controller.Name + ":" + u.Username + "/" + model.Name + "." + offerName)
	c.Assert(err, qt.IsNil)

	offer := dbmodel.ApplicationOffer{
		UUID:            id.String(),
		Name:            offerName,
		ModelID:         model.ID,
		ApplicationName: petname.Generate(2, "-"),
		URL:             offerURL.String(),
	}
	err = db.AddApplicationOffer(context.Background(), &offer)
	c.Assert(err, qt.IsNil)
	c.Assert(len(offer.UUID), qt.Equals, 36)

	return u, group, controller, model, offer, cloud, cred
}

func TestAddGroup(t *testing.T) {
	c := qt.New(t)
	ctx := context.Background()

	ofgaClient, _, _, err := jimmtest.SetupTestOFGAClient(c.Name())
	c.Assert(err, qt.IsNil)

	now := time.Now().UTC().Round(time.Millisecond)
	j := &jimm.JIMM{
		UUID: uuid.NewString(),
		Database: db.Database{
			DB: jimmtest.PostgresDB(c, func() time.Time { return now }),
		},
		OpenFGAClient: ofgaClient,
	}

	err = j.Database.Migrate(ctx, false)
	c.Assert(err, qt.IsNil)

	user, _, _, _, _, _, _ := createTestControllerEnvironment(ctx, c, j.Database)
	u := openfga.NewUser(&user, ofgaClient)
	u.JimmAdmin = true

	err = j.AddGroup(ctx, u, "test-group-1")
	c.Assert(err, qt.IsNil)

	err = j.AddGroup(ctx, u, "test-group-1")
	c.Assert(errors.ErrorCode(err), qt.Equals, errors.CodeAlreadyExists)
}

func TestRemoveGroup(t *testing.T) {
	c := qt.New(t)
	ctx := context.Background()

	ofgaClient, _, _, err := jimmtest.SetupTestOFGAClient(c.Name())
	c.Assert(err, qt.IsNil)

	now := time.Now().UTC().Round(time.Millisecond)
	j := &jimm.JIMM{
		UUID: uuid.NewString(),
		Database: db.Database{
			DB: jimmtest.PostgresDB(c, func() time.Time { return now }),
		},
		OpenFGAClient: ofgaClient,
	}

	err = j.Database.Migrate(ctx, false)
	c.Assert(err, qt.IsNil)

	user, group, _, _, _, _, _ := createTestControllerEnvironment(ctx, c, j.Database)
	u := openfga.NewUser(&user, ofgaClient)
	u.JimmAdmin = true

	err = j.RemoveGroup(ctx, u, group.Name)
	c.Assert(err, qt.IsNil)

	err = j.RemoveGroup(ctx, u, group.Name)
	c.Assert(errors.ErrorCode(err), qt.Equals, errors.CodeNotFound)
}

func TestRemoveGroupRemovesTuples(t *testing.T) {
	c := qt.New(t)
	ctx := context.Background()

	ofgaClient, _, _, err := jimmtest.SetupTestOFGAClient(c.Name())
	c.Assert(err, qt.IsNil)

	now := time.Now().UTC().Round(time.Millisecond)
	j := &jimm.JIMM{
		UUID: uuid.NewString(),
		Database: db.Database{
			DB: jimmtest.PostgresDB(c, func() time.Time { return now }),
		},
		OpenFGAClient: ofgaClient,
	}

	err = j.Database.Migrate(ctx, false)
	c.Assert(err, qt.IsNil)

	user, group, controller, model, _, _, _ := createTestControllerEnvironment(ctx, c, j.Database)

	err = j.Database.AddGroup(ctx, "test-group2")
	c.Assert(err, qt.IsNil)

	group2 := &dbmodel.GroupEntry{
		Name: "test-group2",
	}
	err = j.Database.GetGroup(ctx, group2)
	c.Assert(err, qt.IsNil)

	tuples := []openfga.Tuple{
		//This tuple should remain as it has no relation to group2
		{
			Object:   ofganames.ConvertTag(user.ResourceTag()),
			Relation: "member",
			Target:   ofganames.ConvertTag(group.ResourceTag()),
		},
		// Below tuples should all be removed as they relate to group2
		{
			Object:   ofganames.ConvertTag(user.ResourceTag()),
			Relation: "member",
			Target:   ofganames.ConvertTag(group2.ResourceTag()),
		},
		{
			Object:   ofganames.ConvertTagWithRelation(group2.ResourceTag(), ofganames.MemberRelation),
			Relation: "member",
			Target:   ofganames.ConvertTag(group.ResourceTag()),
		},
		{
			Object:   ofganames.ConvertTagWithRelation(group2.ResourceTag(), ofganames.MemberRelation),
			Relation: "administrator",
			Target:   ofganames.ConvertTag(controller.ResourceTag()),
		},
		{
			Object:   ofganames.ConvertTagWithRelation(group2.ResourceTag(), ofganames.MemberRelation),
			Relation: "writer",
			Target:   ofganames.ConvertTag(model.ResourceTag()),
		},
	}

	err = ofgaClient.AddRelation(ctx, tuples...)
	c.Assert(err, qt.IsNil)

	u := openfga.NewUser(&user, ofgaClient)
	u.JimmAdmin = true

	err = j.RemoveGroup(ctx, u, group.Name)
	c.Assert(err, qt.IsNil)

	err = j.RemoveGroup(ctx, u, group.Name)
	c.Assert(errors.ErrorCode(err), qt.Equals, errors.CodeNotFound)

	remainingTuples, _, err := ofgaClient.ReadRelatedObjects(ctx, ofga.Tuple{}, 0, "")
	c.Assert(err, qt.IsNil)
	c.Assert(remainingTuples, qt.HasLen, 3)

	err = j.RemoveGroup(ctx, u, group2.Name)
	c.Assert(err, qt.IsNil)

	remainingTuples, _, err = ofgaClient.ReadRelatedObjects(ctx, ofga.Tuple{}, 0, "")
	c.Assert(err, qt.IsNil)
	c.Assert(remainingTuples, qt.HasLen, 0)
}

func TestRenameGroup(t *testing.T) {
	c := qt.New(t)
	ctx := context.Background()

	ofgaClient, _, _, err := jimmtest.SetupTestOFGAClient(c.Name())
	c.Assert(err, qt.IsNil)

	now := time.Now().UTC().Round(time.Millisecond)
	j := &jimm.JIMM{
		UUID: uuid.NewString(),
		Database: db.Database{
			DB: jimmtest.PostgresDB(c, func() time.Time { return now }),
		},
		OpenFGAClient: ofgaClient,
	}

	err = j.Database.Migrate(ctx, false)
	c.Assert(err, qt.IsNil)

	user, group, controller, model, _, _, _ := createTestControllerEnvironment(ctx, c, j.Database)

	u := openfga.NewUser(&user, ofgaClient)
	u.JimmAdmin = true

	tuples := []openfga.Tuple{
		{
			Object:   ofganames.ConvertTag(user.ResourceTag()),
			Relation: "member",
			Target:   ofganames.ConvertTag(group.ResourceTag()),
		},
		{
			Object:   ofganames.ConvertTagWithRelation(group.ResourceTag(), ofganames.MemberRelation),
			Relation: "administrator",
			Target:   ofganames.ConvertTag(controller.ResourceTag()),
		},
		{
			Object:   ofganames.ConvertTagWithRelation(group.ResourceTag(), ofganames.MemberRelation),
			Relation: "writer",
			Target:   ofganames.ConvertTag(model.ResourceTag()),
		},
	}

	err = ofgaClient.AddRelation(ctx, tuples...)
	c.Assert(err, qt.IsNil)

	err = j.RenameGroup(ctx, u, group.Name, "test-new-group")
	c.Assert(err, qt.IsNil)

	group.Name = "test-new-group"

	// check the user still has member relation to the group
	allowed, err := ofgaClient.CheckRelation(
		ctx,
		ofga.Tuple{
			Object:   ofganames.ConvertTag(u.ResourceTag()),
			Relation: "member",
			Target:   ofganames.ConvertTag(group.ResourceTag()),
		},
		false,
	)
	c.Assert(err, qt.IsNil)
	c.Assert(allowed, qt.IsTrue)

	// check the user still has writer relation to the model via the
	// group membership
	allowed, err = ofgaClient.CheckRelation(
		ctx,
		ofga.Tuple{
			Object:   ofganames.ConvertTag(u.ResourceTag()),
			Relation: "writer",
			Target:   ofganames.ConvertTag(model.ResourceTag()),
		},
		false,
	)
	c.Assert(err, qt.IsNil)
	c.Assert(allowed, qt.IsTrue)

	// check the user still has administrator relation to the controller
	// via group membership
	allowed, err = ofgaClient.CheckRelation(
		ctx,
		ofga.Tuple{
			Object:   ofganames.ConvertTag(u.ResourceTag()),
			Relation: "administrator",
			Target:   ofganames.ConvertTag(controller.ResourceTag()),
		},
		false,
	)
	c.Assert(err, qt.IsNil)
	c.Assert(allowed, qt.IsTrue)
}

func TestListGroups(t *testing.T) {
	c := qt.New(t)
	ctx := context.Background()

	ofgaClient, _, _, err := jimmtest.SetupTestOFGAClient(c.Name())
	c.Assert(err, qt.IsNil)

	now := time.Now().UTC().Round(time.Millisecond)
	j := &jimm.JIMM{
		UUID: uuid.NewString(),
		Database: db.Database{
			DB: jimmtest.PostgresDB(c, func() time.Time { return now }),
		},
		OpenFGAClient: ofgaClient,
	}

	err = j.Database.Migrate(ctx, false)
	c.Assert(err, qt.IsNil)

	user, group, _, _, _, _, _ := createTestControllerEnvironment(ctx, c, j.Database)

	u := openfga.NewUser(&user, ofgaClient)
	u.JimmAdmin = true

	groups, err := j.ListGroups(ctx, u)
	c.Assert(err, qt.IsNil)
	c.Assert(groups, qt.DeepEquals, []dbmodel.GroupEntry{group})

	groupNames := []string{
		"test-group0",
		"test-group1",
		"test-group2",
		"aaaFinalGroup",
	}

	for _, name := range groupNames {
		err := j.AddGroup(ctx, u, name)
		c.Assert(err, qt.IsNil)
	}

	groups, err = j.ListGroups(ctx, u)
	c.Assert(err, qt.IsNil)
	sort.Slice(groups, func(i, j int) bool {
		return groups[i].Name < groups[j].Name
	})
	c.Assert(groups, qt.HasLen, 5)
	// groups should be returned in ascending order of name
	c.Assert(groups[0].Name, qt.Equals, "aaaFinalGroup")
	c.Assert(groups[1].Name, qt.Equals, group.Name)
	c.Assert(groups[2].Name, qt.Equals, "test-group0")
	c.Assert(groups[3].Name, qt.Equals, "test-group1")
	c.Assert(groups[4].Name, qt.Equals, "test-group2")
}
