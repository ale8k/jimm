// Copyright 2016 Canonical Ltd.

// Package jujuapi implements API endpoints for the juju API.
package jujuapi

import (
	"net/http"

	"github.com/juju/httprequest"
	"github.com/julienschmidt/httprouter"

	"github.com/CanonicalLtd/jem/internal/jem"
	"github.com/CanonicalLtd/jem/internal/jemserver"
)

func NewAPIHandler(jp *jem.Pool, params jemserver.Params) ([]httprequest.Handler, error) {
	return []httprequest.Handler{
		newWebSocketHandler(jp, params),
		newRootWebSocketHandler(jp, params),
	}, nil
}

func newWebSocketHandler(jp *jem.Pool, params jemserver.Params) httprequest.Handler {
	return httprequest.Handler{
		Method: "GET",
		Path:   "/model/:modeluuid/api",
		Handle: func(w http.ResponseWriter, r *http.Request, p httprouter.Params) {
			j := jp.JEM()
			defer j.Close()
			wsServer := newWSServer(j, params, p.ByName("modeluuid"))
			wsServer.ServeHTTP(w, r)
		},
	}
}

func newRootWebSocketHandler(jp *jem.Pool, params jemserver.Params) httprequest.Handler {
	return httprequest.Handler{
		Method: "GET",
		Path:   "/",
		Handle: func(w http.ResponseWriter, r *http.Request, p httprouter.Params) {
			j := jp.JEM()
			defer j.Close()
			wsServer := newWSServer(j, params, "")
			wsServer.ServeHTTP(w, r)
		},
	}
}
