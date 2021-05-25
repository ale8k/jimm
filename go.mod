module github.com/CanonicalLtd/jimm

go 1.16

require (
	github.com/boltdb/bolt v1.3.1 // indirect
	github.com/canonical/candid v1.4.2
	github.com/canonical/go-dqlite v1.8.0
	github.com/canonical/go-service v1.0.0
	github.com/docker/spdystream v0.0.0-20181023171402-6480d4af844c // indirect
	github.com/frankban/quicktest v1.11.3
	github.com/go-macaroon-bakery/macaroon-bakery/v3 v3.0.0-20210309064400-d73aa8f92aa2
	github.com/golang/mock v1.5.0 // indirect
	github.com/google/go-cmp v0.5.4
	github.com/google/uuid v1.1.2
	github.com/gorilla/websocket v1.4.2
	github.com/gosuri/uitable v0.0.1
	github.com/hashicorp/vault/api v1.1.0
	github.com/jackc/pgconn v1.7.0
	github.com/jackc/pgx/v4 v4.9.0
	github.com/juju/charm/v8 v8.0.0-20210510114941-82380ab895dc
	github.com/juju/cmd v0.0.0-20200108104440-8e43f3faa5c9
	github.com/juju/errors v0.0.0-20200330140219-3fe23663418f
	github.com/juju/gnuflag v0.0.0-20171113085948-2ce1bb71843d
	github.com/juju/juju v0.0.0-20210525013243-16f33363aae9
	github.com/juju/loggo v0.0.0-20200526014432-9ce3a2e09b5e
	github.com/juju/mgo/v2 v2.0.0-20210414025616-e854c672032f
	github.com/juju/mgomonitor v0.0.0-20181029151116-52206bb0cd31
	github.com/juju/names/v4 v4.0.0-20200929085019-be23e191fee0
	github.com/juju/rpcreflect v0.0.0-20200416001309-bb46e9ba1476
	github.com/juju/simplekv v1.0.1 // indirect
	github.com/juju/testing v0.0.0-20210324180055-18c50b0c2098
	github.com/juju/utils v0.0.0-20200604140309-9d78121a29e0
	github.com/juju/utils/v2 v2.0.0-20210305225158-eedbe7b6b3e2
	github.com/juju/version v0.0.0-20210303051006-2015802527a8
	github.com/juju/version/v2 v2.0.0-20210319015800-dcfac8f4f057 // indirect
	github.com/juju/zaputil v0.0.0-20190326175239-ef53049637ac
	github.com/julienschmidt/httprouter v1.3.0
	github.com/mattn/go-sqlite3 v2.0.3+incompatible
	github.com/prometheus/client_golang v1.7.1
	github.com/rogpeppe/fastuuid v1.2.0
	go.uber.org/zap v1.10.0
	golang.org/x/crypto v0.0.0-20210506145944-38f3c27a63bf
	golang.org/x/mod v0.4.2 // indirect
	golang.org/x/sync v0.0.0-20210220032951-036812b2e83c
	golang.org/x/term v0.0.0-20210317153231-de623e64d2a6 // indirect
	golang.org/x/tools v0.1.0 // indirect
	gopkg.in/check.v1 v1.0.0-20201130134442-10cb98267c6c
	gopkg.in/errgo.v1 v1.0.1
	gopkg.in/httprequest.v1 v1.2.1
	gopkg.in/juju/environschema.v1 v1.0.1-0.20201027142642-c89a4490670a
	gopkg.in/macaroon-bakery.v2 v2.3.0
	gopkg.in/macaroon.v2 v2.1.0
	gopkg.in/yaml.v2 v2.4.0
	gorm.io/driver/postgres v1.0.5
	gorm.io/driver/sqlite v1.1.4-0.20201029040614-e1caf3738eb9
	gorm.io/gorm v1.20.6
	sigs.k8s.io/yaml v1.2.0
)

replace (
	github.com/altoros/gosigma => github.com/juju/gosigma v0.0.0-20170523021020-a27b59fe2be9
	gopkg.in/yaml.v2 => github.com/juju/yaml v0.0.0-20200420012109-12a32b78de07
)

replace github.com/hashicorp/raft => github.com/juju/raft v1.0.1-0.20190319034642-834fca2f9ffc

replace github.com/mattn/go-sqlite3 => github.com/mattn/go-sqlite3 v1.14.5
