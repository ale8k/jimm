// Copyright 2015-2016 Canonical Ltd.

package admincmd

import (
	"fmt"

	"github.com/juju/cmd"
	"github.com/juju/gnuflag"
	"github.com/juju/juju/cmd/modelcmd"
	"gopkg.in/errgo.v1"

	"github.com/CanonicalLtd/jem/jemclient"
	"github.com/CanonicalLtd/jem/params"
)

type revokeCommand struct {
	commandBase

	path entityPathValue

	controller bool
	users      userSet
}

func newRevokeCommand() cmd.Command {
	return modelcmd.WrapBase(&revokeCommand{})
}

var revokeDoc = `
The revoke command removes permissions for a set of users
or groups to read a model (default) or controller within the managing server.
Note that if a user access is revoked, that user may still have access
if they are a member of a group that is still part of the read ACL.

For example, to remove alice and bob from the read ACL of the model johndoe/mymodel,
assuming they are currently mentioned in the ACL:

    jaas admin revoke johndoe/mymodel alice,bob
`

func (c *revokeCommand) Info() *cmd.Info {
	return &cmd.Info{
		Name:    "revoke",
		Args:    "<user>/<modelname|controllername> username[,username]...",
		Purpose: "revoke permissions of the managing server entity",
		Doc:     revokeDoc,
	}
}

func (c *revokeCommand) SetFlags(f *gnuflag.FlagSet) {
	f.BoolVar(&c.controller, "controller", false, "change ACL of controller not model")
}

func (c *revokeCommand) Init(args []string) error {
	// Validate and store the entity reference.
	if len(args) == 0 {
		return errgo.Newf("no model or controller specified")
	}
	if len(args) == 1 {
		return errgo.Newf("no users specified")
	}
	if len(args) > 2 {
		return errgo.Newf("too many arguments")
	}
	if err := c.path.Set(args[0]); err != nil {
		return errgo.Mask(err)
	}
	c.users = make(userSet)
	if err := c.users.Set(args[1]); err != nil {
		return errgo.Notef(err, "invalid value %q", args[1])
	}
	return nil
}

func (c *revokeCommand) Run(ctxt *cmd.Context) error {
	client, err := c.newClient(ctxt)
	if err != nil {
		return errgo.Mask(err)
	}
	currentACL, err := c.getPerm(client)
	if err != nil {
		return errgo.Mask(err)
	}
	newReadPerms := make(userSet)
	for _, u := range currentACL.Read {
		newReadPerms[u] = true
	}
	for u := range c.users {
		if _, ok := newReadPerms[u]; !ok {
			fmt.Fprintf(ctxt.Stdout, "User %q was not granted before revoke.\n", u)
		} else {
			delete(newReadPerms, u)
		}
	}
	return c.setPerm(client, params.ACL{
		Read: newReadPerms.slice(),
	})
}

func (c *revokeCommand) setPerm(client *jemclient.Client, acl params.ACL) error {
	var err error
	switch {
	case c.controller:
		err = client.SetControllerPerm(&params.SetControllerPerm{
			EntityPath: c.path.EntityPath,
			ACL:        acl,
		})
	default:
		err = client.SetModelPerm(&params.SetModelPerm{
			EntityPath: c.path.EntityPath,
			ACL:        acl,
		})
	}
	return errgo.Mask(err)
}

func (c *revokeCommand) getPerm(client *jemclient.Client) (params.ACL, error) {
	var acl params.ACL
	var err error
	switch {
	case c.controller:
		acl, err = client.GetControllerPerm(&params.GetControllerPerm{
			EntityPath: c.path.EntityPath,
		})
	default:
		acl, err = client.GetModelPerm(&params.GetModelPerm{
			EntityPath: c.path.EntityPath,
		})
	}
	return acl, errgo.Mask(err)
}