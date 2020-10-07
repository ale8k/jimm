// Copyright 2015 Canonical Ltd.

package jem

import (
	"context"
	"fmt"
	"math/rand"
	"path"
	"sort"
	"sync"
	"time"

	vault "github.com/hashicorp/vault/api"
	"github.com/juju/clock"
	"github.com/juju/juju/api"
	"github.com/juju/juju/api/modelmanager"
	jujuparams "github.com/juju/juju/apiserver/params"
	jujucloud "github.com/juju/juju/cloud"
	"github.com/juju/names/v4"
	"github.com/juju/utils/cache"
	"github.com/juju/version"
	"github.com/rogpeppe/fastuuid"
	"go.uber.org/zap"
	"gopkg.in/errgo.v1"
	"gopkg.in/macaroon-bakery.v2/bakery/identchecker"
	"gopkg.in/macaroon-bakery.v2/httpbakery"
	"gopkg.in/mgo.v2"
	"gopkg.in/mgo.v2/bson"

	"github.com/CanonicalLtd/jimm/internal/apiconn"
	"github.com/CanonicalLtd/jimm/internal/auth"
	"github.com/CanonicalLtd/jimm/internal/conv"
	"github.com/CanonicalLtd/jimm/internal/mgosession"
	"github.com/CanonicalLtd/jimm/internal/mongodoc"
	"github.com/CanonicalLtd/jimm/internal/pubsub"
	usageauth "github.com/CanonicalLtd/jimm/internal/usagesender/auth"
	"github.com/CanonicalLtd/jimm/internal/zapctx"
	"github.com/CanonicalLtd/jimm/internal/zaputil"
	"github.com/CanonicalLtd/jimm/params"
)

// wallClock provides access to the current time. It is a variable so
// that it can be overridden in tests.
var wallClock clock.Clock = clock.WallClock

// Functions defined as variables so they can be overridden in tests.
var (
	randIntn = rand.Intn

	NewUsageSenderAuthorizationClient = func(url string, client *httpbakery.Client) (UsageSenderAuthorizationClient, error) {
		return usageauth.NewAuthorizationClient(url, client), nil
	}

	// ModelSummaryWatcherNotSupportedError is returned by WatchAllModelSummaries if
	// the controller does not support this functionality
	ModelSummaryWatcherNotSupportedError = errgo.New("model summary watcher not supported by the controller")
)

// UsageSenderAuthorizationClient is used to obtain authorization to
// collect and report usage metrics.
type UsageSenderAuthorizationClient interface {
	GetCredentials(ctx context.Context, applicationUser string) ([]byte, error)
}

// Params holds parameters for the NewPool function.
type Params struct {
	// DB holds the mongo database that will be used to
	// store the JEM information.
	DB *mgo.Database

	// SessionPool holds a pool from which session objects are
	// taken to be used in database operations.
	SessionPool *mgosession.Pool

	// ControllerAdmin holds the identity of the user
	// or group that is allowed to create controllers.
	ControllerAdmin params.User

	// UsageSenderURL holds the URL where we obtain authorization
	// to collect and report usage metrics.
	UsageSenderURL string

	// Client is used to make the request for usage metrics authorization
	Client *httpbakery.Client

	// PublicCloudMetadata contains the metadata details of all known
	// public clouds.
	PublicCloudMetadata map[string]jujucloud.Cloud

	Pubsub *pubsub.Hub

	// VaultClient is the client for a vault server that is used to store
	// secrets.
	VaultClient *vault.Client

	// VaultPath is the root path in the vault for JIMM's secrets.
	VaultPath string
}

type Pool struct {
	config    Params
	connCache *apiconn.Cache

	// dbName holds the name of the database to use.
	dbName string

	// regionCache caches region information about models
	regionCache *cache.Cache

	// mu guards the fields below it.
	mu sync.Mutex

	// closed holds whether the Pool has been closed.
	closed bool

	// refCount holds the number of JEM instances that
	// currently refer to the pool. The pool is finally
	// closed when all JEM instances are closed and the
	// pool itself has been closed.
	refCount int

	usageSenderAuthorizationClient UsageSenderAuthorizationClient

	// uuidGenerator is used to generate temporary UUIDs during the
	// creation of models, these UUIDs will be replaced with the ones
	// generated by the controllers themselves.
	uuidGenerator *fastuuid.Generator

	pubsub *pubsub.Hub
}

var APIOpenTimeout = 15 * time.Second

var notExistsQuery = bson.D{{"$exists", false}}

// NewPool represents a pool of possible JEM instances that use the given
// database as a store, and use the given bakery parameters to create the
// bakery.Service.
func NewPool(ctx context.Context, p Params) (*Pool, error) {
	// TODO migrate database
	if p.ControllerAdmin == "" {
		return nil, errgo.Newf("no controller admin group specified")
	}
	if p.SessionPool == nil {
		return nil, errgo.Newf("no session pool provided")
	}
	uuidGen, err := fastuuid.NewGenerator()
	if err != nil {
		return nil, errgo.Mask(err)
	}
	pool := &Pool{
		config:        p,
		dbName:        p.DB.Name,
		connCache:     apiconn.NewCache(apiconn.CacheParams{}),
		regionCache:   cache.New(24 * time.Hour),
		refCount:      1,
		uuidGenerator: uuidGen,
		pubsub:        p.Pubsub,
	}
	if pool.config.UsageSenderURL != "" {
		client, err := NewUsageSenderAuthorizationClient(p.UsageSenderURL, p.Client)
		if err != nil {
			return nil, errgo.Notef(err, "cannot make omnibus authorization client")
		}
		pool.usageSenderAuthorizationClient = client
	}
	jem := pool.JEM(ctx)
	defer jem.Close()
	if err := jem.DB.ensureIndexes(); err != nil {
		return nil, errgo.Notef(err, "cannot ensure indexes")
	}
	return pool, nil
}

// Close closes the pool. Its resources will be freed
// when the last JEM instance created from the pool has
// been closed.
func (p *Pool) Close() {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return
	}
	p.decRef()
	p.closed = true
}

func (p *Pool) decRef() {
	// called with p.mu held.
	if p.refCount--; p.refCount == 0 {
		p.connCache.Close()
	}
	if p.refCount < 0 {
		panic("negative reference count")
	}
}

// ClearAPIConnCache clears out the API connection cache.
// This is useful for testing purposes.
func (p *Pool) ClearAPIConnCache() {
	p.connCache.EvictAll()
}

// JEM returns a new JEM instance from the pool, suitable
// for using in short-lived requests. The JEM must be
// closed with the Close method after use.
//
// This method will panic if called after the pool has been
// closed.
func (p *Pool) JEM(ctx context.Context) *JEM {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		panic("JEM call on closed pool")
	}
	p.refCount++
	return &JEM{
		DB:                             newDatabase(ctx, p.config.SessionPool, p.dbName),
		pool:                           p,
		usageSenderAuthorizationClient: p.usageSenderAuthorizationClient,
		pubsub:                         p.pubsub,
	}
}

// UsageAuthorizationClient returns the UsageSenderAuthorizationClient.
func (p *Pool) UsageAuthorizationClient() UsageSenderAuthorizationClient {
	return p.usageSenderAuthorizationClient
}

type JEM struct {
	// DB holds the mongodb-backed identity store.
	DB *Database

	// pool holds the Pool from which the JEM instance
	// was created.
	pool *Pool

	// closed records whether the JEM instance has
	// been closed.
	closed bool

	usageSenderAuthorizationClient UsageSenderAuthorizationClient

	pubsub *pubsub.Hub
}

// Clone returns an independent copy of the receiver
// that uses a cloned database connection. The
// returned value must be closed after use.
func (j *JEM) Clone() *JEM {
	j.pool.mu.Lock()
	defer j.pool.mu.Unlock()

	j.pool.refCount++
	return &JEM{
		DB:   j.DB.clone(),
		pool: j.pool,
	}
}

func (j *JEM) ControllerAdmin() params.User {
	return j.pool.config.ControllerAdmin
}

// Close closes the JEM instance. This should be called when
// the JEM instance is finished with.
func (j *JEM) Close() {
	j.pool.mu.Lock()
	defer j.pool.mu.Unlock()
	if j.closed {
		return
	}
	j.closed = true
	j.DB.Session.Close()
	j.DB = nil
	j.pool.decRef()
}

// Pubsub returns jem's pubsub hub.
func (j *JEM) Pubsub() *pubsub.Hub {
	return j.pubsub
}

// ErrAPIConnection is returned by OpenAPI, OpenAPIFromDoc and
// OpenModelAPI when the API connection cannot be made.
//
// Note that it is defined as an ErrorCode so that Database.checkError
// does not treat it as a mongo-connection-broken error.
var ErrAPIConnection params.ErrorCode = "cannot connect to API"

// OpenAPI opens an API connection to the controller with the given path
// and returns it along with the information used to connect. If the
// controller does not exist, the error will have a cause of
// params.ErrNotFound.
//
// If the controller API connection could not be made, the error will
// have a cause of ErrAPIConnection.
//
// The returned connection must be closed when finished with.
func (j *JEM) OpenAPI(ctx context.Context, path params.EntityPath) (_ *apiconn.Conn, err error) {
	defer j.DB.checkError(ctx, &err)
	ctl, err := j.DB.Controller(ctx, path)
	if err != nil {
		return nil, errgo.NoteMask(err, "cannot get controller", errgo.Is(params.ErrNotFound))
	}
	return j.OpenAPIFromDoc(ctx, ctl)
}

// OpenAPIFromDoc returns an API connection to the controller held in the
// given document. This can be useful when we want to connect to a
// controller before it's added to the database. Note that a successful
// return from this function does not necessarily mean that the
// credentials or API addresses in the docs actually work, as it's
// possible that there's already a cached connection for the given
// controller.
//
// The returned connection must be closed when finished with.
func (j *JEM) OpenAPIFromDoc(ctx context.Context, ctl *mongodoc.Controller) (*apiconn.Conn, error) {
	return j.pool.connCache.OpenAPI(ctx, ctl.UUID, func() (api.Connection, *api.Info, error) {
		info := apiInfoFromDoc(ctl)
		zapctx.Debug(ctx, "open API", zap.Any("api-info", info))
		conn, err := api.Open(info, apiDialOpts())
		if err != nil {
			return nil, nil, errgo.WithCausef(err, ErrAPIConnection, "")
		}
		return conn, info, nil
	})
}

func apiDialOpts() api.DialOpts {
	return api.DialOpts{
		Timeout:    APIOpenTimeout,
		RetryDelay: 500 * time.Millisecond,
	}
}

func apiInfoFromDoc(ctl *mongodoc.Controller) *api.Info {
	return &api.Info{
		Addrs:    mongodoc.Addresses(ctl.HostPorts),
		CACert:   ctl.CACert,
		Tag:      names.NewUserTag(ctl.AdminUser),
		Password: ctl.AdminPassword,
	}
}

// OpenModelAPI opens an API connection to the model with the given path
// and returns it along with the information used to connect. If the
// model does not exist, the error will have a cause of
// params.ErrNotFound.
//
// If the model API connection could not be made, the error will have a
// cause of ErrAPIConnection.
//
// The returned connection must be closed when finished with.
func (j *JEM) OpenModelAPI(ctx context.Context, path params.EntityPath) (_ *apiconn.Conn, err error) {
	defer j.DB.checkError(ctx, &err)
	m := mongodoc.Model{Path: path}
	if err := j.DB.GetModel(ctx, &m); err != nil {
		return nil, errgo.NoteMask(err, "cannot get model", errgo.Is(params.ErrNotFound))
	}
	ctl, err := j.DB.Controller(ctx, m.Controller)
	if err != nil {
		return nil, errgo.Notef(err, "cannot get controller")
	}
	return j.openModelAPIFromDocs(ctx, ctl, &m)
}

// openModelAPIFromDocs returns an API connection to the model held in the
// given documents.
//
// The returned connection must be closed when finished with.
func (j *JEM) openModelAPIFromDocs(ctx context.Context, ctl *mongodoc.Controller, m *mongodoc.Model) (*apiconn.Conn, error) {
	return j.pool.connCache.OpenAPI(ctx, m.UUID, func() (api.Connection, *api.Info, error) {
		info := apiInfoFromDocs(ctl, m)
		zapctx.Debug(ctx, "open API", zap.Any("api-info", info))
		conn, err := api.Open(info, apiDialOpts())
		if err != nil {
			zapctx.Info(ctx, "failed to open connection", zaputil.Error(err), zap.Any("api-info", info))
			return nil, nil, errgo.WithCausef(err, ErrAPIConnection, "")
		}
		return conn, info, nil
	})
}

func apiInfoFromDocs(ctl *mongodoc.Controller, m *mongodoc.Model) *api.Info {
	return &api.Info{
		Addrs:    mongodoc.Addresses(ctl.HostPorts),
		CACert:   ctl.CACert,
		ModelTag: names.NewModelTag(m.UUID),
		Tag:      names.NewUserTag(ctl.AdminUser),
		Password: ctl.AdminPassword,
	}
}

// GetModel retrieves the given model from the database using
// Database.GetModel. It then checks that the given user has the given
// access level on the model. If the model cannot be found then an error
// with a cause of params.ErrNotFound is returned. If the given user
// does not have the correct access level on the model then an error of
// type params.ErrUnauthorized will be returned.
func (j *JEM) GetModel(ctx context.Context, id identchecker.ACLIdentity, access jujuparams.UserAccessPermission, m *mongodoc.Model) error {
	if err := j.DB.GetModel(ctx, m); err != nil {
		return errgo.Mask(err, errgo.Is(params.ErrNotFound))
	}

	// Currently in JAAS the namespace user has full access to the model.
	acl := []string{string(m.Path.User)}
	switch access {
	case jujuparams.ModelReadAccess:
		acl = append(acl, m.ACL.Read...)
		fallthrough
	case jujuparams.ModelWriteAccess:
		acl = append(acl, m.ACL.Write...)
		fallthrough
	case jujuparams.ModelAdminAccess:
		acl = append(acl, m.ACL.Admin...)
	}
	if err := auth.CheckACL(ctx, id, acl); err != nil {
		return errgo.Mask(err, errgo.Is(params.ErrUnauthorized))
	}

	if m.Cloud == "" {
		// The model does not currently store its cloud information so go
		// and fetch it from the model itself. This happens if the model
		// was created with a JIMM version older than 0.9.5.
		if err := j.updateModelInfo(ctx, m); err != nil {
			// Log the failure, but return what we have to the caller.
			zapctx.Error(ctx, "cannot update model info", zap.Error(err))
		}
	}
	return nil
}

// updateModelInfo retrieves model parameters missing in the current database
// from the controller.
func (j *JEM) updateModelInfo(ctx context.Context, model *mongodoc.Model) error {
	conn, err := j.OpenAPI(ctx, model.Controller)
	if err != nil {
		return errgo.Mask(err)
	}
	info := jujuparams.ModelInfo{UUID: model.UUID}
	if err := conn.ModelInfo(ctx, &info); err != nil {
		return errgo.Mask(err)
	}
	cloudTag, err := names.ParseCloudTag(info.CloudTag)
	if err != nil {
		return errgo.Notef(err, "bad data from controller")
	}
	credentialTag, err := names.ParseCloudCredentialTag(info.CloudCredentialTag)
	if err != nil {
		return errgo.Notef(err, "bad data from controller")
	}
	model.Cloud = params.Cloud(cloudTag.Id())
	model.CloudRegion = info.CloudRegion
	owner, err := conv.FromUserTag(credentialTag.Owner())
	if err != nil {
		return errgo.Mask(err, errgo.Is(conv.ErrLocalUser))
	}
	model.Credential = mongodoc.CredentialPath{
		Cloud: string(params.Cloud(credentialTag.Cloud().Id())),
		EntityPath: mongodoc.EntityPath{
			User: string(owner),
			Name: credentialTag.Name(),
		},
	}
	model.DefaultSeries = info.DefaultSeries

	if err := j.DB.UpdateLegacyModel(ctx, model); err != nil {
		return errgo.Mask(err)
	}
	return nil
}

// Controller retrieves the given controller from the database,
// validating that the current user is allowed to read the controller.
func (j *JEM) Controller(ctx context.Context, id identchecker.ACLIdentity, path params.EntityPath) (*mongodoc.Controller, error) {
	if err := j.DB.CheckReadACL(ctx, id, j.DB.Controllers(), path); err != nil {
		return nil, errgo.Mask(err, errgo.Is(params.ErrUnauthorized))
	}
	ctl, err := j.DB.Controller(ctx, path)
	return ctl, errgo.Mask(err, errgo.Is(params.ErrNotFound))
}

// GetCredential retrieves the given credential from the database,
// validating that the current user is allowed to read the credential.
func (j *JEM) GetCredential(ctx context.Context, id identchecker.ACLIdentity, cred *mongodoc.Credential) error {
	if err := j.DB.GetCredential(ctx, cred); err != nil {
		if errgo.Cause(err) == params.ErrNotFound {
			// We return an authorization error for all attempts to retrieve credentials
			// from any other user's space.
			if aerr := auth.CheckIsUser(ctx, id, params.User(cred.Path.User)); aerr != nil {
				err = aerr
			}
		}
		return errgo.Mask(err, errgo.Is(params.ErrNotFound), errgo.Is(params.ErrUnauthorized))
	}
	if err := auth.CheckCanRead(ctx, id, cred); err != nil {
		return errgo.Mask(err, errgo.Is(params.ErrUnauthorized))
	}

	// ensure we always have a provider-type in the credential.
	if cred.ProviderType == "" {
		var err error
		cred.ProviderType, err = j.DB.ProviderType(ctx, params.Cloud(cred.Path.Cloud))
		if err != nil {
			zapctx.Error(ctx, "cannot find provider type for credential", zap.Error(err), zap.Stringer("credential", cred.Path))
		}
	}

	return nil
}

// FillCredentialAttributes ensures that the credential attributes of the
// given credential are set. User access is not checked in this method, it
// is assumed that if the credential is held the user has access.
func (j *JEM) FillCredentialAttributes(ctx context.Context, cred *mongodoc.Credential) error {
	if !cred.AttributesInVault || len(cred.Attributes) > 0 {
		return nil
	}
	if j.pool.config.VaultClient == nil {
		return errgo.New("vault not configured")
	}

	logical := j.pool.config.VaultClient.Logical()
	secret, err := logical.Read(path.Join(j.pool.config.VaultPath, "creds", cred.Path.String()))
	if err != nil {
		return errgo.Mask(err)
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

func (j *JEM) possibleControllers(ctx context.Context, id identchecker.ACLIdentity, ctlPath params.EntityPath, cr *mongodoc.CloudRegion) ([]params.EntityPath, error) {
	if ctlPath.Name != "" {
		return []params.EntityPath{ctlPath}, nil
	}
	if err := j.DB.GetCloudRegion(ctx, cr); err != nil {
		return nil, errgo.Mask(err, errgo.Is(params.ErrNotFound))
	}
	if err := auth.CheckCanRead(ctx, id, cr); err != nil {
		return nil, errgo.Mask(err, errgo.Is(params.ErrUnauthorized))
	}
	shuffle(len(cr.PrimaryControllers), func(i, j int) {
		cr.PrimaryControllers[i], cr.PrimaryControllers[j] = cr.PrimaryControllers[j], cr.PrimaryControllers[i]
	})
	shuffle(len(cr.SecondaryControllers), func(i, j int) {
		cr.SecondaryControllers[i], cr.SecondaryControllers[j] = cr.SecondaryControllers[j], cr.SecondaryControllers[i]
	})
	return append(cr.PrimaryControllers, cr.SecondaryControllers...), nil
}

// shuffle is used to randomize the order in which possible controllers
// are tried. It is a variable so it can be replaced in tests.
var shuffle func(int, func(int, int)) = rand.Shuffle

// RevokeCredential checks that the credential with the given path
// can be revoked (if flags&CredentialCheck!=0) and revokes
// the credential (if flags&CredentialUpdate!=0).
// If flags==0, it acts as if both CredentialCheck and CredentialUpdate
// were set.
func (j *JEM) RevokeCredential(ctx context.Context, credPath params.CredentialPath, flags CredentialUpdateFlags) error {
	if flags == 0 {
		flags = ^0
	}
	cred := mongodoc.Credential{
		Path: mongodoc.CredentialPathFromParams(credPath),
	}
	if err := j.DB.GetCredential(ctx, &cred); err != nil {
		return errgo.Mask(err, errgo.Is(params.ErrNotFound))
	}
	controllers := cred.Controllers
	if flags&CredentialCheck != 0 {
		models, err := j.DB.ModelsWithCredential(ctx, mongodoc.CredentialPathFromParams(credPath))
		if err != nil {
			return errgo.Mask(err)
		}
		if len(models) > 0 {
			// TODO more informative error message.
			return errgo.Newf("cannot revoke because credential is in use on at least one model")
		}
	}
	if flags&CredentialUpdate == 0 {
		return nil
	}
	if err := j.DB.updateCredential(ctx, &mongodoc.Credential{
		Path:    mongodoc.CredentialPathFromParams(credPath),
		Revoked: true,
	}); err != nil {
		return errgo.Notef(err, "cannot update local database")
	}
	ch := make(chan struct{}, len(controllers))
	n := len(controllers)
	for _, ctlPath := range controllers {
		ctlPath, j := ctlPath, j.Clone()
		go func() {
			defer func() {
				ch <- struct{}{}
			}()
			defer j.Close()
			conn, err := j.OpenAPI(ctx, ctlPath)
			if err != nil {
				zapctx.Warn(ctx,
					"cannot connect to controller to revoke credential",
					zap.String("controller", ctlPath.String()),
					zaputil.Error(err),
				)
				return
			}
			defer conn.Close()

			err = j.revokeControllerCredential(ctx, conn, ctlPath, cred.Path.ToParams())
			if err != nil {
				zapctx.Warn(ctx,
					"cannot revoke credential",
					zap.String("controller", ctlPath.String()),
					zaputil.Error(err),
				)
			}
		}()
	}
	for n > 0 {
		select {
		case <-ch:
			n--
		case <-ctx.Done():
			return errgo.Notef(ctx.Err(), "timed out revoking credentials")
		}
	}
	return nil
}

type CredentialUpdateFlags int

const (
	CredentialUpdate CredentialUpdateFlags = 1 << iota
	CredentialCheck
)

// UpdateCredential checks that the credential can be updated (if the
// CredentialUpdate flag is set) and updates its in the local database
// and all controllers to which it is deployed (if the CredentialCheck
// flag is specified).
//
// If flags is zero, it will both check and update.
func (j *JEM) UpdateCredential(ctx context.Context, cred *mongodoc.Credential, flags CredentialUpdateFlags) ([]jujuparams.UpdateCredentialModelResult, error) {
	if cred.Revoked {
		return nil, errgo.Newf("cannot use UpdateCredential to revoke a credential")
	}
	if flags == 0 {
		flags = ^0
	}
	var controllers []params.EntityPath
	c := mongodoc.Credential{
		Path: cred.Path,
	}
	if err := j.DB.GetCredential(ctx, &c); err == nil {
		controllers = c.Controllers
	} else if errgo.Cause(err) != params.ErrNotFound {
		return nil, errgo.Mask(err)
	}
	if flags&CredentialCheck != 0 {
		// There is a credential already recorded, so check with all its controllers
		// that it's valid before we update it locally and update it on the controllers.
		models, err := j.checkCredential(ctx, cred, controllers)
		if err != nil || flags&CredentialUpdate == 0 {
			return models, errgo.Mask(err, apiconn.IsAPIError)
		}
	}

	// Try to ensure that we set the provider type.
	cred.ProviderType = c.ProviderType
	if cred.ProviderType == "" {
		var err error
		cred.ProviderType, err = j.DB.ProviderType(ctx, params.Cloud(cred.Path.Cloud))
		if err != nil {
			zapctx.Warn(ctx, "cannot determine provider type", zap.Error(err), zap.String("cloud", cred.Path.Cloud))
		}
	}

	// Note that because CredentialUpdate is checked for inside the
	// CredentialCheck case above, we know that we need to
	// update the credential in this case.
	models, err := j.updateCredential(ctx, cred, controllers)
	return models, errgo.Mask(err, apiconn.IsAPIError)
}

func (j *JEM) updateCredential(ctx context.Context, cred *mongodoc.Credential, controllers []params.EntityPath) ([]jujuparams.UpdateCredentialModelResult, error) {
	// The credential has now been checked (or we're going
	// to force the update), so update it in the local database.
	// and mark in the local database that an update is required for
	// all controllers
	if j.pool.config.VaultClient != nil {
		// There is a vault, so store the actual credential in there.
		cred1 := *cred
		cred1.Attributes = nil
		cred1.AttributesInVault = true
		if err := j.DB.updateCredential(ctx, &cred1); err != nil {
			return nil, errgo.Notef(err, "cannot update local database")
		}
		data := make(map[string]interface{}, len(cred.Attributes))
		for k, v := range cred.Attributes {
			data[k] = v
		}
		logical := j.pool.config.VaultClient.Logical()
		_, err := logical.Write(path.Join(j.pool.config.VaultPath, "creds", cred.Path.String()), data)
		if err != nil {
			return nil, errgo.Mask(err)
		}
	} else {
		cred.AttributesInVault = false
		if err := j.DB.updateCredential(ctx, cred); err != nil {
			return nil, errgo.Notef(err, "cannot update local database")
		}
	}
	if err := j.DB.setCredentialUpdates(ctx, cred.Controllers, cred.Path); err != nil {
		return nil, errgo.Notef(err, "cannot mark controllers to be updated")
	}

	// Attempt to update all controllers to which the credential is
	// deployed. If these fail they will be updated by the monitor.
	// Make the channel buffered so we don't leak go-routines
	ch := make(chan updateCredentialResult, len(controllers))
	for _, ctlPath := range controllers {
		ctlPath, j := ctlPath, j.Clone()
		go func() {
			defer j.Close()
			conn, err := j.OpenAPI(ctx, ctlPath)
			if err != nil {
				ch <- updateCredentialResult{
					ctlPath: ctlPath,
					err:     errgo.Mask(err),
				}
				return
			}
			defer conn.Close()

			models, err := j.updateControllerCredential(ctx, conn, ctlPath, cred)
			ch <- updateCredentialResult{
				ctlPath: ctlPath,
				models:  models,
				err:     errgo.Mask(err, apiconn.IsAPIError),
			}
		}()
	}
	models, err := mergeUpdateCredentialResults(ctx, ch, len(controllers))
	return models, errgo.Mask(err, apiconn.IsAPIError)
}

func (j *JEM) checkCredential(ctx context.Context, newCred *mongodoc.Credential, controllers []params.EntityPath) ([]jujuparams.UpdateCredentialModelResult, error) {
	if len(controllers) == 0 {
		// No controllers, so there's nowhere to check that the credential
		// is valid.
		return nil, nil
	}
	ch := make(chan updateCredentialResult, len(controllers))
	for _, ctlPath := range controllers {
		ctlPath, j := ctlPath, j.Clone()
		go func() {
			defer j.Close()
			models, err := j.checkCredentialOnController(ctx, ctlPath, newCred)
			ch <- updateCredentialResult{ctlPath, models, errgo.Mask(err, apiconn.IsAPIError)}
		}()
	}
	models, err := mergeUpdateCredentialResults(ctx, ch, len(controllers))
	return models, errgo.Mask(err, apiconn.IsAPIError)
}

type updateCredentialResult struct {
	ctlPath params.EntityPath
	models  []jujuparams.UpdateCredentialModelResult
	err     error
}

func mergeUpdateCredentialResults(ctx context.Context, ch <-chan updateCredentialResult, n int) ([]jujuparams.UpdateCredentialModelResult, error) {
	var models []jujuparams.UpdateCredentialModelResult
	var firstError error
	for n > 0 {
		select {
		case r := <-ch:
			n--
			models = append(models, r.models...)
			if r.err != nil {
				zapctx.Warn(ctx,
					"cannot update credential",
					zap.String("controller", r.ctlPath.String()),
					zaputil.Error(r.err),
				)
				if firstError == nil {
					firstError = errgo.NoteMask(r.err, fmt.Sprintf("controller %s", r.ctlPath), apiconn.IsAPIError)
				}
			}

		case <-ctx.Done():
			return nil, errgo.Notef(ctx.Err(), "timed out checking credentials")
		}
	}
	return models, errgo.Mask(firstError, apiconn.IsAPIError)
}

func (j *JEM) checkCredentialOnController(ctx context.Context, ctlPath params.EntityPath, cred *mongodoc.Credential) ([]jujuparams.UpdateCredentialModelResult, error) {
	conn, err := j.OpenAPI(ctx, ctlPath)
	if err != nil {
		return nil, errgo.Mask(err)
	}
	defer conn.Close()

	if !conn.SupportsCheckCredentialModels() {
		// Version 3 of the Cloud facade isn't supported, so there is nothing to do.
		return nil, nil
	}
	models, err := conn.CheckCredentialModels(ctx, cred)
	return models, errgo.Mask(err, apiconn.IsAPIError)
}

// updateControllerCredential updates the given credential (which must
// not be revoked) on the given controller.
// If rp is non-nil, it will be updated with information
// on the models updated.
func (j *JEM) updateControllerCredential(
	ctx context.Context,
	conn *apiconn.Conn,
	ctlPath params.EntityPath,
	cred *mongodoc.Credential,
) ([]jujuparams.UpdateCredentialModelResult, error) {
	if cred.Revoked {
		return nil, errgo.New("updateControllerCredential called with revoked credential (shouldn't happen)")
	}
	if err := j.FillCredentialAttributes(ctx, cred); err != nil {
		return nil, errgo.Mask(err)
	}
	models, err := conn.UpdateCredential(ctx, cred)
	if err == nil {
		if dberr := j.DB.clearCredentialUpdate(ctx, ctlPath, cred.Path); dberr != nil {
			err = errgo.Notef(dberr, "cannot update controller %q after successfully updating credential", ctlPath)
		}
	}
	if err != nil {
		err = errgo.NoteMask(err, "cannot update credentials", apiconn.IsAPIError)
	}
	return models, err
}

func (j *JEM) revokeControllerCredential(
	ctx context.Context,
	conn *apiconn.Conn,
	ctlPath params.EntityPath,
	credPath params.CredentialPath,
) error {
	if err := conn.RevokeCredential(ctx, credPath); err != nil {
		return errgo.Mask(err, apiconn.IsAPIError)
	}
	if err := j.DB.clearCredentialUpdate(ctx, ctlPath, mongodoc.CredentialPathFromParams(credPath)); err != nil {
		return errgo.Notef(err, "cannot update controller %q after successfully updating credential", ctlPath)
	}
	return nil
}

// GrantModel grants the given access for the given user on the given model and updates the JEM database.
func (j *JEM) GrantModel(ctx context.Context, conn *apiconn.Conn, model *mongodoc.Model, user params.User, access string) error {
	if err := conn.GrantModelAccess(ctx, model.UUID, user, jujuparams.UserAccessPermission(access)); err != nil {
		return errgo.Mask(err, apiconn.IsAPIError)
	}
	if err := j.DB.GrantModel(ctx, model.Path, user, access); err != nil {
		return errgo.Mask(err)
	}
	return nil
}

// RevokeModel revokes the given access for the given user on the given model and updates the JEM database.
func (j *JEM) RevokeModel(ctx context.Context, conn *apiconn.Conn, model *mongodoc.Model, user params.User, access string) error {
	if err := j.DB.RevokeModel(ctx, model.Path, user, access); err != nil {
		return errgo.Mask(err)
	}
	if err := conn.RevokeModelAccess(ctx, model.UUID, user, jujuparams.UserAccessPermission(access)); err != nil {
		// TODO (mhilton) What should be done with the changes already made to the database.
		return errgo.Mask(err, apiconn.IsAPIError)
	}
	return nil
}

// EarliestControllerVersion returns the earliest agent version
// that any of the available public controllers is known to be running.
// If there are no available controllers or none of their versions are
// known, it returns the zero version.
func (j *JEM) EarliestControllerVersion(ctx context.Context, id identchecker.ACLIdentity) (version.Number, error) {
	// TOD(rog) cache the result of this for a while, as it changes only rarely
	// and we don't really need to make this extra round trip every
	// time a user connects to the API?
	var v *version.Number
	if err := j.doControllers(ctx, id, func(c *mongodoc.Controller) error {
		zapctx.Debug(ctx, "in EarliestControllerVersion", zap.Stringer("controller", c.Path), zap.Stringer("version", c.Version))
		if c.Version == nil {
			return nil
		}
		if v == nil || c.Version.Compare(*v) < 0 {
			v = c.Version
		}
		return nil
	}); err != nil {
		return version.Number{}, errgo.Mask(err)
	}
	if v == nil {
		return version.Number{}, nil
	}
	return *v, nil
}

// doControllers calls the given function for each controller that
// can be read by the current user that matches the given attributes.
// If the function returns an error, the iteration stops and
// doControllers returns the error with the same cause.
//
// Note that the same pointer is passed to the do function on
// each iteration. It is the responsibility of the do function to
// copy it if needed.
func (j *JEM) doControllers(ctx context.Context, id identchecker.ACLIdentity, do func(c *mongodoc.Controller) error) error {
	// Query all the controllers that match the attributes, building
	// up all the possible values.
	q := j.DB.Controllers().Find(bson.D{{"unavailablesince", notExistsQuery}, {"public", true}})
	// Sort by _id so that we can make easily reproducible tests.
	iter := j.DB.NewCanReadIter(id, q.Sort("_id").Iter())
	var ctl mongodoc.Controller
	for iter.Next(ctx, &ctl) {
		if err := do(&ctl); err != nil {
			iter.Close(ctx)
			return errgo.Mask(err, errgo.Any)
		}
	}
	if err := iter.Err(ctx); err != nil {
		return errgo.Notef(err, "cannot query")
	}
	return nil
}

// UpdateMachineInfo updates the information associated with a machine.
func (j *JEM) UpdateMachineInfo(ctx context.Context, ctlPath params.EntityPath, info *jujuparams.MachineInfo) error {
	cloud, region, err := j.modelRegion(ctx, ctlPath, info.ModelUUID)
	if errgo.Cause(err) == params.ErrNotFound {
		// If the model isn't found then it is not controlled by
		// JIMM and we aren't interested in it.
		return nil
	}
	if err != nil {
		return errgo.Notef(err, "cannot find region for model %s:%s", ctlPath, info.ModelUUID)
	}
	return errgo.Mask(j.DB.UpdateMachineInfo(ctx, &mongodoc.Machine{
		Controller: ctlPath.String(),
		Cloud:      cloud,
		Region:     region,
		Info:       info,
	}))
}

// UpdateApplicationInfo updates the information associated with an application.
func (j *JEM) UpdateApplicationInfo(ctx context.Context, ctlPath params.EntityPath, info *jujuparams.ApplicationInfo) error {
	cloud, region, err := j.modelRegion(ctx, ctlPath, info.ModelUUID)
	if errgo.Cause(err) == params.ErrNotFound {
		// If the model isn't found then it is not controlled by
		// JIMM and we aren't interested in it.
		return nil
	}
	if err != nil {
		return errgo.Notef(err, "cannot find region for model %s:%s", ctlPath, info.ModelUUID)
	}
	app := &mongodoc.Application{
		Controller: ctlPath.String(),
		Cloud:      cloud,
		Region:     region,
	}
	if info != nil {
		app.Info = &mongodoc.ApplicationInfo{
			ModelUUID:       info.ModelUUID,
			Name:            info.Name,
			Exposed:         info.Exposed,
			CharmURL:        info.CharmURL,
			OwnerTag:        info.OwnerTag,
			Life:            info.Life,
			Subordinate:     info.Subordinate,
			Status:          info.Status,
			WorkloadVersion: info.WorkloadVersion,
		}
	}
	return errgo.Mask(j.DB.UpdateApplicationInfo(ctx, app))
}

// modelRegion determines the cloud and region in which a model is contained.
func (j *JEM) modelRegion(ctx context.Context, ctlPath params.EntityPath, uuid string) (params.Cloud, string, error) {
	type cloudRegion struct {
		cloud  params.Cloud
		region string
	}
	key := fmt.Sprintf("%s %s", ctlPath, uuid)
	r, err := j.pool.regionCache.Get(key, func() (interface{}, error) {
		m, err := j.DB.modelFromControllerAndUUID(ctx, ctlPath, uuid)
		if err != nil {
			return nil, errgo.Mask(err, errgo.Is(params.ErrNotFound))
		}
		return cloudRegion{
			cloud:  m.Cloud,
			region: m.CloudRegion,
		}, nil
	})
	if err != nil {
		return "", "", errgo.Mask(err, errgo.Is(params.ErrNotFound))
	}
	cr := r.(cloudRegion)
	return cr.cloud, cr.region, nil
}

// UpdateModelCredential updates the credential used with a model on both
// the controller and the local database.
func (j *JEM) UpdateModelCredential(ctx context.Context, conn *apiconn.Conn, model *mongodoc.Model, cred *mongodoc.Credential) error {
	if _, err := j.updateControllerCredential(ctx, conn, model.Controller, cred); err != nil {
		return errgo.Notef(err, "cannot add credential")
	}
	if err := j.DB.credentialAddController(ctx, cred.Path, model.Controller); err != nil {
		return errgo.Notef(err, "cannot add credential")
	}

	client := modelmanager.NewClient(conn)
	if err := client.ChangeModelCredential(names.NewModelTag(model.UUID), conv.ToCloudCredentialTag(cred.Path.ToParams())); err != nil {
		return errgo.Mask(err)
	}

	if err := j.DB.SetModelCredential(ctx, model.Path, cred.Path); err != nil {
		return errgo.Mask(err)
	}
	return nil
}

func (j *JEM) MongoVersion(ctx context.Context) (jujuparams.StringResult, error) {
	result := jujuparams.StringResult{}
	binfo, err := j.pool.config.DB.Session.BuildInfo()
	if err != nil {
		return result, errgo.Mask(err)
	}
	result.Result = binfo.Version
	return result, nil
}

func plural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}

// WatchAllModelSummaries starts watching the summary updates from
// the controller.
func (j *JEM) WatchAllModelSummaries(ctx context.Context, ctlPath params.EntityPath) (func() error, error) {
	conn, err := j.OpenAPI(ctx, ctlPath)
	if err != nil {
		return nil, errgo.Mask(err)
	}

	if !conn.SupportsModelSummaryWatcher() {
		return nil, ModelSummaryWatcherNotSupportedError
	}
	id, err := conn.WatchAllModelSummaries(ctx)
	if err != nil {
		errgo.Mask(err, apiconn.IsAPIError)
	}
	watcher := &modelSummaryWatcher{
		conn:    conn,
		id:      id,
		pubsub:  j.pubsub,
		cleanup: conn.Close,
	}
	go watcher.loop(ctx)
	return watcher.stop, nil
}

type modelSummaryWatcher struct {
	conn    *apiconn.Conn
	id      string
	pubsub  *pubsub.Hub
	cleanup func() error
}

func (w *modelSummaryWatcher) next(ctx context.Context) ([]jujuparams.ModelAbstract, error) {
	models, err := w.conn.ModelSummaryWatcherNext(ctx, w.id)
	if err != nil {
		return nil, errgo.Mask(err, apiconn.IsAPIError)
	}
	sort.Slice(models, func(i, j int) bool {
		return models[i].UUID < models[j].UUID
	})
	return models, nil
}

func (w *modelSummaryWatcher) loop(ctx context.Context) {
	defer func() {
		if err := w.cleanup(); err != nil {
			zapctx.Error(ctx, "cleanup failed", zaputil.Error(err))
		}
	}()

	for {
		select {
		case <-ctx.Done():
			return
		default:
		}
		modelSummaries, err := w.next(ctx)
		if err != nil {
			zapctx.Error(ctx, "failed to get next model summary", zaputil.Error(err))
			return
		}
		for _, modelSummary := range modelSummaries {
			w.pubsub.Publish(modelSummary.UUID, modelSummary)
		}
	}
}

func (w *modelSummaryWatcher) stop() error {
	return errgo.Mask(w.conn.ModelSummaryWatcherStop(context.TODO(), w.id), apiconn.IsAPIError)
}
