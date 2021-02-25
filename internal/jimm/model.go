// Copyright 2020 Canonical Ltd.

package jimm

import (
	"context"
	"database/sql"
	"fmt"
	"math/rand"
	"path"
	"sort"
	"strings"
	"time"

	jujuparams "github.com/juju/juju/apiserver/params"
	"github.com/juju/names/v4"
	"go.uber.org/zap"

	"github.com/CanonicalLtd/jimm/internal/dbmodel"
	"github.com/CanonicalLtd/jimm/internal/errors"
	"github.com/CanonicalLtd/jimm/internal/zapctx"
	"github.com/CanonicalLtd/jimm/internal/zaputil"
)

// shuffle is used to randomize the order in which possible controllers
// are tried. It is a variable so it can be replaced in tests.
var shuffle func(int, func(int, int)) = rand.Shuffle

func shuffleRegionControllers(controllers []dbmodel.CloudRegionControllerPriority) {
	shuffle(len(controllers), func(i, j int) {
		controllers[i], controllers[j] = controllers[j], controllers[i]
	})
	sort.SliceStable(controllers, func(i, j int) bool {
		return controllers[i].Priority > controllers[j].Priority
	})
}

// ModelCreateArgs contains parameters used to add a new model.
type ModelCreateArgs struct {
	Name            string
	Owner           names.UserTag
	Config          map[string]interface{}
	Cloud           names.CloudTag
	CloudRegion     string
	CloudCredential names.CloudCredentialTag
}

// FromJujuModelCreateArgs convers jujuparams.ModelCreateArgs into AddModelArgs.
func (a *ModelCreateArgs) FromJujuModelCreateArgs(args *jujuparams.ModelCreateArgs) error {
	if args.Name == "" {
		return errors.E("name not specified")
	}
	a.Name = args.Name
	a.Config = args.Config
	a.CloudRegion = args.CloudRegion
	if args.CloudTag == "" {
		return errors.E("cloud tag not specified")
	}
	ct, err := names.ParseCloudTag(args.CloudTag)
	if err != nil {
		return errors.E(err, "invalid cloud tag")
	}
	a.Cloud = ct

	if args.OwnerTag == "" {
		return errors.E("owner tag not specified")
	}
	ot, err := names.ParseUserTag(args.OwnerTag)
	if err != nil {
		return errors.E(err, "invalid owner tag")
	}
	a.Owner = ot

	if args.CloudCredentialTag != "" {
		ct, err := names.ParseCloudCredentialTag(args.CloudCredentialTag)
		if err != nil {
			return errors.E(err, "invalid cloud credential tag")
		}
		if a.Cloud.Id() != "" && ct.Cloud().Id() != a.Cloud.Id() {
			return errors.E("cloud credential cloud mismatch")
		}

		a.CloudCredential = ct
	}
	return nil
}

func newModelBuilder(ctx context.Context, j *JIMM) *modelBuilder {
	return &modelBuilder{
		ctx:  ctx,
		jimm: j,
	}
}

type modelBuilder struct {
	ctx context.Context
	err error

	jimm *JIMM

	name          string
	config        map[string]interface{}
	user          *dbmodel.User
	owner         *dbmodel.User
	credential    *dbmodel.CloudCredential
	controller    *dbmodel.Controller
	cloud         *dbmodel.Cloud
	cloudRegion   string
	cloudRegionID uint
	model         *dbmodel.Model
	modelInfo     *jujuparams.ModelInfo
}

// Error returns the error that occured in the process
// of adding a new model.
func (b *modelBuilder) Error() error {
	return b.err
}

func (b *modelBuilder) jujuModelCreateArgs() (*jujuparams.ModelCreateArgs, error) {
	if b.name == "" {
		return nil, errors.E("model name not specified")
	}
	if b.owner == nil {
		return nil, errors.E("model owner not specified")
	}
	if b.cloud == nil {
		return nil, errors.E("cloud not specified")
	}
	if b.cloudRegionID == 0 {
		return nil, errors.E("cloud region not specified")
	}
	if b.credential == nil {
		return nil, errors.E("credentials not specified")
	}

	return &jujuparams.ModelCreateArgs{
		Name:               b.name,
		OwnerTag:           b.owner.Tag().String(),
		Config:             b.config,
		CloudTag:           b.cloud.Tag().String(),
		CloudRegion:        b.cloudRegion,
		CloudCredentialTag: b.credential.Tag().String(),
	}, nil

}

// WithUser returns a builder with the authorized user.
func (b *modelBuilder) WithUser(user *dbmodel.User) *modelBuilder {
	b.user = user
	if b.owner != nil && b.owner.Username != user.Username && user.ControllerAccess != "superuser" {
		b.err = errors.E(errors.CodeUnauthorized)
	}
	return b
}

// WithOwner returns a builder with the specified owner.
func (b *modelBuilder) WithOwner(owner names.UserTag) *modelBuilder {
	if b.err != nil {
		return b
	}
	if b.user != nil && owner.Id() != b.user.Username && b.user.ControllerAccess != "superuser" {
		b.err = errors.E(errors.CodeUnauthorized)
	}

	user := dbmodel.User{
		Username: owner.Id(),
	}
	err := b.jimm.Database.GetUser(b.ctx, &user)
	if err != nil {
		b.err = err
	}
	b.owner = &user
	return b
}

// WithName returns a builder with the specified model name.
func (b *modelBuilder) WithName(name string) *modelBuilder {

	b.name = name
	return b
}

// WithConfig returns a builder with the specified model config.
func (b *modelBuilder) WithConfig(cfg map[string]interface{}) *modelBuilder {
	b.config = cfg
	return b
}

// WithCloud returns a builder with the specified cloud.
func (b *modelBuilder) WithCloud(cloud names.CloudTag) *modelBuilder {
	if b.err != nil {
		return b
	}
	c := dbmodel.Cloud{
		Name: cloud.Id(),
	}

	if err := b.jimm.Database.GetCloud(b.ctx, &c); err != nil {
		b.err = err
		return b
	}
	b.cloud = &c
	return b
}

// WithCloudRegion returns a builder with the specified cloud region.
func (b *modelBuilder) WithCloudRegion(region string) *modelBuilder {
	if b.err != nil {
		return b
	}
	if b.cloud != nil {
		// loop through all cloud regions
		for _, r := range b.cloud.Regions {
			// if the region matches
			if r.Name == region {
				// consider all possible controlers for that region
				regionControllers := r.Controllers
				if len(regionControllers) == 0 {
					b.err = errors.E(errors.CodeBadRequest, fmt.Sprintf("unsupported cloud region %s/%s", b.cloud.Name, region))
				}
				// shuffle controllers
				shuffleRegionControllers(regionControllers)

				// and sellect the first controller in the slice
				b.cloudRegion = region
				b.cloudRegionID = regionControllers[0].CloudRegionID
				b.controller = &regionControllers[0].Controller

				break
			}
		}
		// we looped through all cloud regions and could not find a match
		if b.cloudRegionID == 0 {
			b.err = errors.E(fmt.Sprintf("cloud region %s not found", region))
		}
	} else {
		b.err = errors.E("cloud not specified")
	}
	return b
}

// WithCloudCredential returns a builder with the specified cloud credentials.
func (b *modelBuilder) WithCloudCredential(credentialTag names.CloudCredentialTag) *modelBuilder {
	if b.err != nil {
		return b
	}
	credential := dbmodel.CloudCredential{
		Name:      credentialTag.Name(),
		CloudName: credentialTag.Cloud().Id(),
		OwnerID:   credentialTag.Owner().Id(),
	}
	err := b.jimm.Database.GetCloudCredential(b.ctx, &credential)
	if err != nil {
		b.err = errors.E(err, fmt.Sprintf("failed to fetch cloud credentials %s", credential.Path()))
	}
	b.credential = &credential
	return b
}

// CreateDatabaseModel stores temporary model information.
func (b *modelBuilder) CreateDatabaseModel() *modelBuilder {
	if b.err != nil {
		return b
	}

	// if model name is not specified we error and abort
	if b.name == "" {
		b.err = errors.E("model name not specified")
		return b
	}
	// if the model owner is not specified we error and abort
	if b.owner == nil {
		b.err = errors.E("owner not specified")
		return b
	}
	// if at this point the cloud region is not specified we
	// try to select a region/controller among the available
	// regions/controllers for the specified cloud
	if b.cloudRegionID == 0 {
		// if selectCloudRegion returns an error that means we have
		// no regions/controllers for the specified cloud - we
		// error and abort
		if err := b.selectCloudRegion(); err != nil {
			b.err = errors.E(err)
			return b
		}
	}
	// if controller is still not selected, there's nothing
	// we can do - either a cloud or a cloud region was specified
	// by this point and a controller should've been selected
	if b.controller == nil {
		b.err = errors.E("unable to determine a suitable controller")
	}

	if b.credential == nil {
		// try to select a valid credential
		if err := b.selectCloudCredentials(); err != nil {
			b.err = errors.E(err, "could not select cloud credentials")
		}
	}

	b.model = &dbmodel.Model{
		Name:              b.name,
		ControllerID:      b.controller.ID,
		Owner:             *b.owner,
		CloudCredentialID: b.credential.ID,
		CloudRegionID:     b.cloudRegionID,
	}

	err := b.jimm.Database.AddModel(b.ctx, b.model)
	if err != nil {
		if errors.ErrorCode(err) == errors.CodeAlreadyExists {
			b.err = errors.E(err, fmt.Sprintf("model %s/%s already exists", b.owner.Username, b.name))
			return b
		} else {
			zapctx.Error(b.ctx, "failed to store model information", zaputil.Error(err))
			b.err = errors.E(err, "failed to store model information")
		}
	}
	return b
}

// Cleanup deletes temporary model information if there was an
// error in the process of creating model.
func (b *modelBuilder) Cleanup() {
	if b.err == nil {
		return
	}
	if b.model == nil {
		return
	}
	if derr := b.jimm.Database.DeleteModel(b.ctx, b.model); derr != nil {
		zapctx.Error(b.ctx, "failed to delete model", zap.String("model", b.model.Name), zap.String("owner", b.model.Owner.Username), zaputil.Error(derr))
	}
}

func (b *modelBuilder) UpdateDatabaseModel() *modelBuilder {
	if b.err != nil {
		return b
	}
	err := b.model.FromJujuModelInfo(b.modelInfo)
	if err != nil {
		b.err = errors.E(err, "failed to convert model info")
		return b
	}
	b.model.ControllerID = b.controller.ID
	// we know which credentials and cloud region was used
	// - ignore this information returned by the controller
	//   because we need IDs to properly update the model
	b.model.CloudCredentialID = b.credential.ID
	b.model.CloudRegionID = b.cloudRegionID
	b.model.CloudCredential = dbmodel.CloudCredential{}
	b.model.CloudRegion = dbmodel.CloudRegion{}

	err = b.filterModelUserAccesses()
	if err != nil {
		b.err = errors.E(err)
		return b
	}

	err = b.jimm.Database.UpdateModel(b.ctx, b.model)
	if err != nil {
		b.err = errors.E(err, "failed to store model information")
		return b
	}
	return b
}

func (b *modelBuilder) selectCloudRegion() error {
	if b.cloudRegionID != 0 {
		return nil
	}
	if b.cloud == nil {
		return errors.E("cloud not specified")
	}

	var regionControllers []dbmodel.CloudRegionControllerPriority
	for _, r := range b.cloud.Regions {
		regionControllers = append(regionControllers, r.Controllers...)
	}

	// if no controllers are found, we return an error
	if len(regionControllers) == 0 {
		return errors.E(fmt.Sprintf("unsupported cloud %s", b.cloud.Name))
	}

	// shuffle controllers according to their priority
	shuffleRegionControllers(regionControllers)

	b.cloudRegionID = regionControllers[0].CloudRegionID
	b.controller = &regionControllers[0].Controller

	return nil
}

func (b *modelBuilder) selectCloudCredentials() error {
	if b.user == nil {
		return errors.E("user not specified")
	}
	if b.cloud == nil {
		return errors.E("cloud not specified")
	}
	credentials, err := b.jimm.Database.GetUserCloudCredentials(b.ctx, b.user, b.cloud.Name)
	if err != nil {
		return errors.E(err, "failed to fetch user cloud credentials")
	}
	for _, credential := range credentials {
		// consider only valid credentials
		if credential.Valid.Valid && credential.Valid.Bool == true {
			b.credential = &credential
			return nil
		}
	}
	return errors.E("valid cloud credentials not found")
}

func (b *modelBuilder) filterModelUserAccesses() error {
	a := []dbmodel.UserModelAccess{}
	for _, access := range b.model.Users {
		access := access

		// JIMM users will contain an @ sign in the username
		if !strings.Contains(access.User.Username, "@") {
			continue
		}

		// fetch user information
		if err := b.jimm.Database.GetUser(b.ctx, &access.User); err != nil {
			return errors.E(err)
		}
		a = append(a, access)
	}
	b.model.Users = a
	return nil
}

// CreateControllerModel uses provided information to create a new
// model on the selected controller.
func (b *modelBuilder) CreateControllerModel() *modelBuilder {
	if b.err != nil {
		return b
	}

	if b.model == nil {
		b.err = errors.E("model not specified")
		return b
	}

	api, err := b.jimm.dial(b.ctx, b.controller, names.ModelTag{})
	if err != nil {
		b.err = errors.E(err)
		return b
	}
	defer api.Close()

	if b.credential != nil {
		if err := b.updateCredential(b.ctx, api, b.credential); err != nil {
			b.err = errors.E("failed to update cloud credential", err)
			return b
		}
	}

	args, err := b.jujuModelCreateArgs()
	if err != nil {
		b.err = errors.E(err)
		return b
	}

	var info jujuparams.ModelInfo
	if err := api.CreateModel(b.ctx, args, &info); err != nil {
		switch jujuparams.ErrCode(err) {
		case jujuparams.CodeAlreadyExists:
			// The model already exists in the controller but it didn't
			// exist in the database. This probably means that it's
			// been abortively created previously, but left around because
			// of connection failure.
			// it's empty, but return an error to the user because
			// TODO initiate cleanup of the model, first checking that
			// the operation to delete a model isn't synchronous even
			// for empty models. We could also have a worker that deletes
			// empty models that don't appear in the database.
			b.err = errors.E(err, errors.CodeAlreadyExists, "model name in use")
		case jujuparams.CodeUpgradeInProgress:
			b.err = errors.E(err, "upgrade in progress")
		default:
			// The model couldn't be created because of an
			// error in the request, don't try another
			// controller.
			b.err = errors.E(err, errors.CodeBadRequest)
		}
		return b
	}

	// Grant JIMM admin access to the model. Note that if this fails,
	// the local database entry will be deleted but the model
	// will remain on the controller and will trigger the "already exists
	// in the backend controller" message above when the user
	// attempts to create a model with the same name again.
	if err := api.GrantJIMMModelAdmin(b.ctx, names.NewModelTag(info.UUID)); err != nil {
		zapctx.Error(b.ctx, "leaked model", zap.String("model", info.UUID), zaputil.Error(err))
		b.err = errors.E(err)
		return b
	}

	b.modelInfo = &info
	return b
}

func (b *modelBuilder) updateCredential(ctx context.Context, api API, cred *dbmodel.CloudCredential) error {
	if err := b.fillCredentialAttributes(ctx, cred); err != nil {
		return errors.E(err)
	}

	update := jujuparams.TaggedCredential{
		Tag: names.NewCloudCredentialTag(cred.Path()).String(),
		Credential: jujuparams.CloudCredential{
			AuthType:   cred.AuthType,
			Attributes: cred.Attributes,
		},
	}
	_, err := api.UpdateCredential(ctx, update)
	if err != nil {
		return errors.E(err)
	}
	return nil
}

// fillCredentialAttributes ensures that the credential attributes of the
// given credential are set. User access is not checked in this method, it
// is assumed that if the credential is held the user has access.
func (b *modelBuilder) fillCredentialAttributes(ctx context.Context, cred *dbmodel.CloudCredential) error {
	if !cred.AttributesInVault || len(cred.Attributes) > 0 {
		return nil
	}
	if b.jimm.VaultClient == nil {
		return errors.E("vault not configured")
	}

	logical := b.jimm.VaultClient.Logical()
	secret, err := logical.Read(path.Join(b.jimm.VaultPath, "creds", cred.Path()))
	if err != nil {
		return errors.E(err)
	}
	if secret == nil {
		// secret will be nil if it is not there. Return an error if we
		// Don't expect the attributes to be empty.
		if cred.AuthType == "empty" {
			return nil
		}
		return errors.E("credential attributes not found")
	}
	cred.Attributes = make(map[string]string, len(secret.Data))
	for k, v := range secret.Data {
		// Nothing will be stored that isn't a string, so ignore anything
		// that is a different type.
		s, ok := v.(string)
		if !ok {
			continue
		}
		cred.Attributes[k] = s
	}
	return nil
}

// JujuModelInfo returns model information returned by the controller.
func (b *modelBuilder) JujuModelInfo() *jujuparams.ModelInfo {
	return b.modelInfo
}

// AddModel adds the specified model to JIMM.
func (j *JIMM) AddModel(ctx context.Context, u *dbmodel.User, args *ModelCreateArgs) (_ *jujuparams.ModelInfo, err error) {
	const op = errors.Op("jimm.AddModel")

	builder := newModelBuilder(ctx, j)
	builder = builder.WithName(args.Name)
	builder = builder.WithConfig(args.Config)
	builder = builder.WithUser(u)
	if err := builder.Error(); err != nil {
		return nil, errors.E(op, err)
	}
	builder = builder.WithOwner(args.Owner)
	if err := builder.Error(); err != nil {
		return nil, errors.E(op, err)
	}
	if args.Cloud != (names.CloudTag{}) {
		builder = builder.WithCloud(args.Cloud)
		if err := builder.Error(); err != nil {
			return nil, errors.E(op, err)
		}
	}
	if args.CloudRegion != "" {
		builder = builder.WithCloudRegion(args.CloudRegion)
		if err := builder.Error(); err != nil {
			return nil, errors.E(op, err)
		}
	}
	if args.CloudCredential != (names.CloudCredentialTag{}) {
		builder = builder.WithCloudCredential(args.CloudCredential)
		if err := builder.Error(); err != nil {
			return nil, errors.E(op, err)
		}
	}
	builder = builder.CreateDatabaseModel()
	if err := builder.Error(); err != nil {
		return nil, errors.E(op, err)
	}
	defer builder.Cleanup()

	builder = builder.CreateControllerModel()
	if err := builder.Error(); err != nil {
		return nil, errors.E(op, err)
	}

	builder = builder.UpdateDatabaseModel()
	if err := builder.Error(); err != nil {
		return nil, errors.E(op, err)
	}
	return builder.JujuModelInfo(), nil
}

// ModelInfo returns the model info for the model with the given ModelTag.
// The returned ModelInfo will be appropriate for the given user's
// access-level on the model. If the model does not exist then the returned
// error will have the code CodeNotFound. If the given user does not have
// access to the model then the returned error will have the code
// CodeUnauthorized.
func (j *JIMM) ModelInfo(ctx context.Context, u *dbmodel.User, mt names.ModelTag) (*jujuparams.ModelInfo, error) {
	const op = errors.Op("jimm.ModelInfo")

	m := dbmodel.Model{
		UUID: sql.NullString{
			String: mt.Id(),
			Valid:  true,
		},
	}

	if err := j.Database.GetModel(ctx, &m); err != nil {
		return nil, errors.E(op, err)
	}

	mi := m.ToJujuModelInfo()
	var modelUser jujuparams.ModelUserInfo
	for _, user := range mi.Users {
		if user.UserName == u.Username {
			modelUser = user
		}
	}

	if u.ControllerAccess == "superuser" || modelUser.Access == "admin" {
		// Admin users have access to all data unmodified.
		return &mi, nil
	}

	if modelUser.Access == "" {
		// If the user doesn't have any access on the model return an
		// unauthorized error
		return nil, errors.E(op, errors.CodeUnauthorized)
	}

	// Non-admin users can only see their own user.
	mi.Users = []jujuparams.ModelUserInfo{modelUser}

	if modelUser.Access != "write" {
		// Users need "write" level access (or above) to see machine
		// information. Note "admin" level users will have already
		// returned data above.
		mi.Machines = nil
	}

	return &mi, nil
}

// ModelStatus returns a jujuparams.ModelStatus for the given model. If
// the model doesn't exist then the returned error will have the code
// CodeNotFound, If the given user does not have admin access to the model
// then the returned error will have the code CodeUnauthorized.
func (j *JIMM) ModelStatus(ctx context.Context, u *dbmodel.User, mt names.ModelTag) (*jujuparams.ModelStatus, error) {
	const op = errors.Op("jimm.ModelStatus")

	m := dbmodel.Model{
		UUID: sql.NullString{
			String: mt.Id(),
			Valid:  true,
		},
	}

	if err := j.Database.GetModel(ctx, &m); err != nil {
		return nil, errors.E(op, err)
	}

	if u.ControllerAccess != "superuser" && m.UserAccess(u) != "admin" {
		// If the user doesn't have admin access on the model return
		// an unauthorized error.
		return nil, errors.E(op, errors.CodeUnauthorized)
	}

	// ModelStatus contains fields that aren't exported over watchers
	// and is only available to admin users, so always fetch from
	// the controller.
	api, err := j.dial(ctx, &m.Controller, names.ModelTag{})
	if err != nil {
		return nil, errors.E(op, err)
	}
	defer api.Close()

	ms := jujuparams.ModelStatus{
		ModelTag: m.Tag().String(),
	}
	if err := api.ModelStatus(ctx, &ms); err != nil {
		return nil, errors.E(op, err)
	}
	return &ms, nil
}

// ForEachUserModel calls the given function once for each model that the
// given user has been granted explicit access to. The UserModelAccess
// object passed to f will always include the Model_, Access, and
// LastConnection fields populated. ForEachUserModel ignores a user's
// controller access when determining the set of models to return, for
// superusers the ForEachModel method should be used to get every model
// in the system. If the given function returns an error the error will
// be returned unmodified and iteration will stop immediately.
func (j *JIMM) ForEachUserModel(ctx context.Context, u *dbmodel.User, f func(*dbmodel.UserModelAccess) error) error {
	const op = errors.Op("jimm.ForEachUserModel")

	models, err := j.Database.GetUserModels(ctx, u)
	if err != nil {
		return errors.E(op, err)
	}

	for _, m := range models {
		switch m.Access {
		default:
			continue
		case "read", "write", "admin":
		}
		if err := f(&m); err != nil {
			return err
		}
	}
	return nil
}

// ForEachModel calls the given function once for each model in the
// system. The UserModelAccess object passed to f will always specify
// that the user's Access is "admin" and will not include the
// LastConnection tim. ForEachModel will return an error with the code
// CodeUnauthorized when the user is not a controller admin. If the given
// function returns an error the error will be returned unmodified and
// iteration will stop immediately.
func (j *JIMM) ForEachModel(ctx context.Context, u *dbmodel.User, f func(*dbmodel.UserModelAccess) error) error {
	const op = errors.Op("jimm.ForEachUserModel")

	if u.ControllerAccess != "superuser" {
		return errors.E(op, errors.CodeUnauthorized)
	}

	errStop := errors.E("stop")
	var iterErr error
	err := j.Database.ForEachModel(ctx, func(m *dbmodel.Model) error {
		uma := dbmodel.UserModelAccess{
			Access: "admin",
			Model_: *m,
		}
		if err := f(&uma); err != nil {
			iterErr = err
			return errStop
		}
		return nil
	})
	switch err {
	case nil:
		return nil
	case errStop:
		return iterErr
	default:
		return errors.E(op, err)
	}
}

// GrantModelAccess grants the given access level on the given model to
// the given user. If the model is not found then an error with the code
// CodeNotFound is returned. If the authenticated user does not have
// admin access to the model then an error with the code CodeUnauthorized
// is returned. If the ModifyModelAccess API call retuns an error the
// error code is not masked.
func (j *JIMM) GrantModelAccess(ctx context.Context, u *dbmodel.User, mt names.ModelTag, ut names.UserTag, access jujuparams.UserAccessPermission) error {
	const op = errors.Op("jimm.GrantModelAccess")

	m := dbmodel.Model{
		UUID: sql.NullString{
			String: mt.Id(),
			Valid:  true,
		},
	}

	if err := j.Database.GetModel(ctx, &m); err != nil {
		return errors.E(op, err)
	}

	if u.ControllerAccess != "superuser" && m.UserAccess(u) != "admin" {
		// If the user doesn't have admin access on the model return
		// an unauthorized error.
		return errors.E(op, errors.CodeUnauthorized)
	}
	targetUser := dbmodel.User{
		Username: ut.Id(),
	}
	if err := j.Database.GetUser(ctx, &targetUser); err != nil {
		return errors.E(op, err)
	}

	// Attempt to grant access to the user on the controller, the
	// controller will validate the request so there is no need to do
	// it here.
	api, err := j.dial(ctx, &m.Controller, names.ModelTag{})
	if err != nil {
		return errors.E(op, err)
	}
	defer api.Close()
	if err := api.GrantModelAccess(ctx, mt, ut, access); err != nil {
		return errors.E(op, err)
	}

	// The change on the controller has succeeded, update the local
	// database.
	var uma dbmodel.UserModelAccess
	for _, a := range m.Users {
		if a.UserID == targetUser.ID {
			uma = a
			break
		}
	}
	uma.User = targetUser
	uma.Model_ = m
	uma.Access = string(access)

	if err := j.Database.UpdateUserModelAccess(ctx, &uma); err != nil {
		return errors.E(op, err, "cannot update database after updating controller")
	}
	return nil
}

// RevokeModelAccess revokes the given access level on the given model
// from the given user. If the model is not found then an error with the
// code CodeNotFound is returned. If the authenticated user does not
// have admin access to the model then an error with the code
// CodeUnauthorized is returned. If the ModifyModelAccess API call
// retuns an error the error code is not masked.
func (j *JIMM) RevokeModelAccess(ctx context.Context, u *dbmodel.User, mt names.ModelTag, ut names.UserTag, access jujuparams.UserAccessPermission) error {
	const op = errors.Op("jimm.RevokeModelAccess")

	m := dbmodel.Model{
		UUID: sql.NullString{
			String: mt.Id(),
			Valid:  true,
		},
	}

	if err := j.Database.GetModel(ctx, &m); err != nil {
		return errors.E(op, err)
	}

	if u.ControllerAccess != "superuser" && m.UserAccess(u) != "admin" {
		// If the user doesn't have admin access on the model return
		// an unauthorized error.
		return errors.E(op, errors.CodeUnauthorized)
	}
	targetUser := dbmodel.User{
		Username: ut.Id(),
	}
	if err := j.Database.GetUser(ctx, &targetUser); err != nil {
		return errors.E(op, err)
	}

	// Attempt to revoke access to the user on the controller, the
	// controller will validate the request so there is no need to do
	// it here.
	api, err := j.dial(ctx, &m.Controller, names.ModelTag{})
	if err != nil {
		return errors.E(op, err)
	}
	defer api.Close()
	if err := api.RevokeModelAccess(ctx, mt, ut, access); err != nil {
		return errors.E(op, err)
	}

	// The change on the controller has succeeded, update the local
	// database.
	var uma dbmodel.UserModelAccess
	for _, a := range m.Users {
		if a.UserID == targetUser.ID {
			uma = a
			break
		}
	}
	uma.User = targetUser
	uma.Model_ = m
	switch access {
	case "admin":
		uma.Access = "write"
	case "write":
		uma.Access = "read"
	default:
		uma.Access = ""
	}

	if err := j.Database.UpdateUserModelAccess(ctx, &uma); err != nil {
		return errors.E(op, err, "cannot update database after updating controller")
	}
	return nil
}

// DestroyModel starts the process of destroying the given model. If the
// given user is not a controller superuser or a model admin an error
// with a code of CodeUnauthorized is returned. Any error returned from
// the juju API will not have it's code masked.
func (j *JIMM) DestroyModel(ctx context.Context, u *dbmodel.User, mt names.ModelTag, destroyStorage, force *bool, maxWait *time.Duration) error {
	const op = errors.Op("jimm.DestroyModel")

	m := dbmodel.Model{
		UUID: sql.NullString{
			String: mt.Id(),
			Valid:  true,
		},
	}

	if err := j.Database.GetModel(ctx, &m); err != nil {
		return errors.E(op, err)
	}
	if u.ControllerAccess != "superuser" && m.UserAccess(u) != "admin" {
		// If the user doesn't have admin access on the model return
		// an unauthorized error.
		return errors.E(op, errors.CodeUnauthorized)
	}

	api, err := j.dial(ctx, &m.Controller, names.ModelTag{})
	if err != nil {
		return errors.E(op, err)
	}
	defer api.Close()
	if err := api.DestroyModel(ctx, mt, destroyStorage, force, maxWait); err != nil {
		return errors.E(op, err)
	}

	m.Life = "dying"
	if err := j.Database.UpdateModel(ctx, &m); err != nil {
		// If the database fails to update don't worry too much the
		// monitor should catch it.
		zapctx.Error(ctx, "failed to store model change", zaputil.Error(err))
	}

	return nil
}
