#!/usr/bin/env python3
# This file is part of the JIMM k8s Charm for Juju.
# Copyright 2022 Canonical Ltd.
#
# This program is free software: you can redistribute it and/or modify
# it under the terms of the GNU General Public License version 3, as
# published by the Free Software Foundation.
#
# This program is distributed in the hope that it will be useful, but
# WITHOUT ANY WARRANTY; without even the implied warranties of
# MERCHANTABILITY, SATISFACTORY QUALITY, or FITNESS FOR A PARTICULAR
# PURPOSE.  See the GNU General Public License for more details.
#
# You should have received a copy of the GNU General Public License
# along with this program. If not, see <http://www.gnu.org/licenses/>.


import functools
import hashlib
import json
import logging
import os

import hvac
import pgsql
from charmhelpers.contrib.charmsupport.nrpe import NRPE
from charms.nginx_ingress_integrator.v0.ingress import IngressRequires
from ops import pebble
from ops.charm import CharmBase
from ops.framework import StoredState
from ops.main import main
from ops.model import (
    ActiveStatus,
    BlockedStatus,
    MaintenanceStatus,
    ModelError,
    WaitingStatus,
)

logger = logging.getLogger(__name__)

WORKLOAD_CONTAINER = "jimm"

REQUIRED_SETTINGS = ["JIMM_UUID", "JIMM_DNS_NAME", "JIMM_DSN", "CANDID_URL"]


def log_event_handler(method):
    @functools.wraps(method)
    def decorated(self, event):
        logger.debug("running {}".format(method.__name__))
        try:
            return method(self, event)
        finally:
            logger.debug("completed {}".format(method.__name__))

    return decorated


class JimmOperatorCharm(CharmBase):
    """JIMM Operator Charm."""

    _stored = StoredState()

    def __init__(self, *args):
        super().__init__(*args)
        self.framework.observe(
            self.on.jimm_pebble_ready, self._on_jimm_pebble_ready
        )
        self.framework.observe(self.on.config_changed, self._on_config_changed)
        self.framework.observe(self.on.update_status, self._on_update_status)
        self.framework.observe(self.on.leader_elected, self._on_leader_elected)
        self.framework.observe(self.on.start, self._on_start)
        self.framework.observe(self.on.stop, self._on_stop)
        self.framework.observe(
            self.on.nrpe_relation_joined, self._on_nrpe_relation_joined
        )
        self.framework.observe(
            self.on.website_relation_joined, self._on_website_relation_joined
        )
        self.framework.observe(
            self.on.dashboard_relation_joined,
            self._on_dashboard_relation_joined,
        )

        # ingress relation
        self.ingress = IngressRequires(
            self,
            {
                "service-hostname": self.config.get("dns-name", ""),
                "service-name": self.app.name,
                "service-port": 8080,
            },
        )

        self._stored.set_default(db_uri=None)

        # database relation
        self.db = pgsql.PostgreSQLClient(self, "db")
        self.framework.observe(
            self.db.on.database_relation_joined,
            self._on_database_relation_joined,
        )
        self.framework.observe(
            self.db.on.master_changed, self._on_master_changed
        )

        self._local_agent_filename = "agent.json"
        self._local_vault_secret_filename = "vault_secret.js"
        self._agent_filename = "/root/config/agent.json"
        self._vault_secret_filename = "/root/config/vault_secret.json"
        self._dashboard_path = "/root/dashboard"
        self._dashboard_hash_path = "/root/dashboard/hash"

    @log_event_handler
    def _on_jimm_pebble_ready(self, event):
        self._update_workload(event)

    @log_event_handler
    def _on_config_changed(self, event):
        self._update_workload(event)

    @log_event_handler
    def _on_leader_elected(self, event):
        self._update_workload(event)

    @log_event_handler
    def _on_website_relation_joined(self, event):
        """Connect a website relation."""

        # we set the port in the unit bucket.
        event.relation.data[self.unit]["port"] = "8080"

    @log_event_handler
    def _on_nrpe_relation_joined(self, event):
        """Connect a NRPE relation."""

        # use the nrpe library to handle the relation.
        nrpe = NRPE()
        nrpe.add_check(
            shortname="JIMM",
            description="check JIMM running",
            check_cmd="check_http -w 2 -c 10 -I {} -p 8080 -u /debug/info".format(
                self.model.get_binding(event.relation).network.ingress_address,
            ),
        )
        nrpe.write()

    def _ensure_bakery_agent_file(self, event):
        # we create the file containing agent keys if needed.
        if not self._path_exists_in_workload(self._agent_filename):
            url = self.config.get("candid-url", "")
            username = self.config.get("candid-agent-username", "")
            private_key = self.config.get("candid-agent-private-key", "")
            public_key = self.config.get("candid-agent-public-key", "")
            if not url or not username or not private_key or not public_key:
                return ""
            data = {
                "key": {"public": public_key, "private": private_key},
                "agents": [{"url": url, "username": username}],
            }
            agent_data = json.dumps(data)

            self._push_to_workload(self._agent_filename, agent_data, event)

    def _ensure_vault_config(self, event):
        addr = self.config.get("vault-url", "")
        if not addr:
            return

        # we create the file containing vault secretes if needed.
        if not self._path_exists_in_workload(self._vault_secret_filename):
            role_id = self.config.get("vault-role-id", "")
            if not role_id:
                return
            token = self.config.get("vault-token", "")
            if not token:
                return
            client = hvac.Client(url=addr, token=token)
            secret = client.sys.unwrap()
            secret["data"]["role_id"] = role_id

            secret_data = json.dumps(secret)
            self._push_to_workload(
                self._vault_secret_filename, secret_data, event
            )

    def _update_workload(self, event):
        """' Update workload with all available configuration
        data."""

        container = self.unit.get_container(WORKLOAD_CONTAINER)
        if not container.can_connect():
            logger.info(
                "cannot connect to the workload container - deffering the event"
            )
            event.defer()
            return

        self._ensure_bakery_agent_file(event)
        self._ensure_vault_config(event)
        self._install_dashboard(event)

        self.ingress.update_config(
            {"service-hostname": self.config.get("dns-name", "")}
        )

        config_values = {
            "CANDID_PUBLIC_KEY": self.config.get("candid-public-key", ""),
            "CANDID_URL": self.config.get("candid-url", ""),
            "JIMM_ADMINS": self.config.get("controller-admins", ""),
            "JIMM_DNS_NAME": self.config.get("dns-name", ""),
            "JIMM_LOG_LEVEL": self.config.get("log-level", ""),
            "JIMM_UUID": self.config.get("uuid", ""),
            "JIMM_DASHBOARD_LOCATION": self.config.get(
                "juju-dashboard-location", "https://jaas.ai/models"
            ),
            "JIMM_LISTEN_ADDR": ":8080",
        }
        if self._stored.db_uri:
            config_values["JIMM_DSN"] = "pgx:{}".format(self._stored.db_uri)

        if container.exists(self._agent_filename):
            config_values["BAKERY_AGENT_FILE"] = self._agent_filename

        if container.exists(self._vault_secret_filename):
            config_values["VAULT_ADDR"] = self.config.get("vault-url", "")
            config_values["VAULT_PATH"] = "charm-jimm-creds"
            config_values["VAULT_SECRET_FILE"] = self._vault_secret_filename
            config_values["VAULT_AUTH_PATH"] = "/auth/approle/login"

        if self.model.unit.is_leader():
            config_values["JIMM_WATCH_CONTROLLERS"] = "1"

        if container.exists(self._dashboard_path):
            config_values["JIMM_DASHBOARD_LOCATION"] = self._dashboard_path

        # remove empty configuration values
        config_values = {
            key: value for key, value in config_values.items() if value
        }

        pebble_layer = {
            "summary": "jimm layer",
            "description": "pebble config layer for jimm",
            "services": {
                "jimm": {
                    "override": "merge",
                    "summary": "JAAS Intelligent Model Manager",
                    "command": "/root/jimmsrv",
                    "startup": "disabled",
                    "environment": config_values,
                }
            },
            "checks": {
                "jimm-check": {
                    "override": "replace",
                    "period": "1m",
                    "http": {"url": "http://localhost:8080/debug/status"},
                }
            },
        }
        container.add_layer("jimm", pebble_layer, combine=True)
        if self._ready():
            if container.get_service("jimm").is_running():
                container.replan()
            else:
                container.start("jimm")
            self.unit.status = ActiveStatus("running")
        else:
            logger.info("workload container not ready - defering")
            event.defer()

        dashboard_relation = self.model.get_relation("dashboard")
        if dashboard_relation:
            dashboard_relation.data[self.app].update(
                {
                    "controller-url": self.config["dns-name"],
                    "identity-provider-url": self.config["candid-url"],
                    "is-juju": str(False),
                }
            )

    @log_event_handler
    def _on_start(self, _):
        """Start JIMM."""
        container = self.unit.get_container(WORKLOAD_CONTAINER)
        if container.can_connect():
            plan = container.get_plan()
            if plan.services.get("jimm") is None:
                logger.error("waiting for service")
                self.unit.status = WaitingStatus("waiting for service")
                return False

            env_vars = plan.services.get("jimm").environment
            for setting in REQUIRED_SETTINGS:
                if not env_vars.get(setting, ""):
                    self.unit.status = BlockedStatus(
                        "{} configuration value not set".format(setting),
                    )
                    return False
            container.start("jimm")

    @log_event_handler
    def _on_stop(self, _):
        """Stop JIMM."""
        container = self.unit.get_container(WORKLOAD_CONTAINER)
        if container.can_connect():
            container.stop()
        self._ready()

    @log_event_handler
    def _on_update_status(self, _):
        """Update the status of the charm."""
        self._ready()

    @log_event_handler
    def _on_dashboard_relation_joined(self, event):
        event.relation.data[self.app].update(
            {
                "controller-url": self.config["dns-name"],
                "identity-provider-url": self.config["candid-url"],
                "is-juju": str(False),
            }
        )

    def _on_database_relation_joined(
        self, event: pgsql.DatabaseRelationJoinedEvent
    ) -> None:
        """
        Handles determining if the database has finished setup, once setup is complete
        a master/standby may join / change in consequent events.
        """
        logging.info("(postgresql) RELATION_JOINED event fired.")

        if self.model.unit.is_leader():
            event.database = "jimm"
        elif event.database != "jimm":
            event.defer()

    def _on_master_changed(self, event: pgsql.MasterChangedEvent) -> None:
        """
        Handles master units of postgres joining / changing.
        The internal snap configuration is updated to reflect this.
        """
        logging.info("(postgresql) MASTER_CHANGED event fired.")

        if event.database != "jimm":
            logging.debug("Database setup not complete yet, returning.")
            return

        if event.master:
            self._stored.db_uri = str(event.master.uri)

    def _ready(self):
        container = self.unit.get_container(WORKLOAD_CONTAINER)

        if container.can_connect():
            plan = container.get_plan()
            if plan.services.get("jimm") is None:
                logger.error("waiting for service")
                self.unit.status = WaitingStatus("waiting for service")
                return False

            env_vars = plan.services.get("jimm").environment

            for setting in REQUIRED_SETTINGS:
                if not env_vars.get(setting, ""):
                    self.unit.status = BlockedStatus(
                        "{} configuration value not set".format(setting),
                    )
                    return False

            if container.get_service("jimm").is_running():
                self.unit.status = ActiveStatus("running")
            else:
                self.unit.status = WaitingStatus("stopped")
            return True
        else:
            logger.error("cannot connect to workload container")
            self.unit.status = WaitingStatus("waiting for jimm workload")
            return False

    def _install_dashboard(self, event):
        container = self.unit.get_container(WORKLOAD_CONTAINER)

        # if we can't connect to the container we should defer
        # this event.
        if not container.can_connect():
            event.defer()

        # fetch the resource filename
        try:
            dashboard_file = self.model.resources.fetch("dashboard")
        except ModelError:
            dashboard_file = None

        # if the resource is not specified, we can return
        # as there is nothing to install.
        if not dashboard_file:
            return

        # if the resource file is empty, we can return
        # as there is nothing to install.
        if os.path.getsize(dashboard_file) == 0:
            return

        dashboard_changed = False

        # compute the hash of the dashboard tarball.
        dashboard_hash = self._hash(dashboard_file)

        # check if we the file containing the dashboard
        # hash exists.
        if container.exists(self._dashboard_hash_path):
            # if it does, compare the stored hash with the
            # hash of the dashboard tarball.
            hash = container.pull(self._dashboard_hash_path)
            existing_hash = str(hash.read())
            # if the two hashes do not match
            # the resource must have changed.
            if not dashboard_hash == existing_hash:
                dashboard_changed = True
        else:
            dashboard_changed = True

        # if the resource file has not changed, we can
        # return as there is no need to push the same
        # dashboard content to the container.
        if not dashboard_changed:
            return

        self.unit.status = MaintenanceStatus("installing dashboard")

        # remove the existing dashboard from the workload/
        if container.exists(self._dashboard_path):
            container.remove_path(self._dashboard_path)

        container.make_dir(self._dashboard_path, make_parents=True)

        with open(dashboard_file, "rb") as f:
            container.push(
                os.path.join(self._dashboard_path, "dashboard.tar.bz2"), f
            )

        process = container.exec(
            [
                "tar",
                "xvf",
                os.path.join(self._dashboard_path, "dashboard.tar.bz2"),
                "-C",
                self._dashboard_path,
            ]
        )
        try:
            process.wait_output()
        except pebble.ExecError as e:
            logger.error(
                "error running untaring the dashboard. error code {}".format(
                    e.exit_code
                )
            )
            for line in e.stderr.splitlines():
                logger.error("    %s", line)

        self._push_to_workload(
            self._dashboard_hash_path, dashboard_hash, event
        )

    def _path_exists_in_workload(self, path: str):
        """Returns true if the specified path exists in the
        workload container."""
        container = self.unit.get_container(WORKLOAD_CONTAINER)
        if container.can_connect():
            return container.exists(path)
        return False

    def _push_to_workload(self, filename, content, event):
        """Create file on the workload container with
        the specified content."""

        container = self.unit.get_container(WORKLOAD_CONTAINER)
        if container.can_connect():
            logger.info(
                "pushing file {} to the workload containe".format(filename)
            )
            container.push(filename, content, make_dirs=True)
        else:
            logger.info("workload container not ready - defering")
            event.defer()

    def _hash(self, filename):
        BUF_SIZE = 65536
        md5 = hashlib.md5()

        with open(filename, "rb") as f:
            while True:
                data = f.read(BUF_SIZE)
                if not data:
                    break
                md5.update(data)
            return md5.hexdigest()


if __name__ == "__main__":
    main(JimmOperatorCharm)