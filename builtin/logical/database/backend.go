package database

import (
	"fmt"
	"strings"
	"sync"

	log "github.com/mgutz/logxi/v1"

	"github.com/hashicorp/vault/builtin/logical/database/dbplugin"
	"github.com/hashicorp/vault/logical"
	"github.com/hashicorp/vault/logical/framework"
)

const databaseConfigPath = "database/config/"

func Factory(conf *logical.BackendConfig) (logical.Backend, error) {
	return Backend(conf).Setup(conf)
}

func Backend(conf *logical.BackendConfig) *databaseBackend {
	var b databaseBackend
	b.Backend = &framework.Backend{
		Help: strings.TrimSpace(backendHelp),

		Paths: []*framework.Path{
			pathConfigurePluginConnection(&b),
			pathListRoles(&b),
			pathRoles(&b),
			pathRoleCreate(&b),
			pathResetConnection(&b),
		},

		Secrets: []*framework.Secret{
			secretCreds(&b),
		},

		Clean: b.closeAllDBs,

		Invalidate: b.invalidate,
	}

	b.logger = conf.Logger
	b.connections = make(map[string]dbplugin.Database)
	return &b
}

type databaseBackend struct {
	connections map[string]dbplugin.Database
	logger      log.Logger

	*framework.Backend
	sync.Mutex
}

// resetAllDBs closes all connections from all database types
func (b *databaseBackend) closeAllDBs() {
	b.Lock()
	defer b.Unlock()

	for _, db := range b.connections {
		db.Close()
	}

	b.connections = make(map[string]dbplugin.Database)
}

// This function is used to retrieve a database object either from the cached
// connection map or by using the database config in storage. The caller of this
// function needs to hold the backend's lock.
func (b *databaseBackend) getOrCreateDBObj(s logical.Storage, name string) (dbplugin.Database, error) {
	// if the object already is built and cached, return it
	db, ok := b.connections[name]
	if ok {
		return db, nil
	}

	config, err := b.DatabaseConfig(s, name)
	if err != nil {
		return nil, err
	}

	db, err = dbplugin.PluginFactory(config.PluginName, b.System(), b.logger)
	if err != nil {
		return nil, err
	}

	err = db.Initialize(config.ConnectionDetails, true)
	if err != nil {
		return nil, err
	}

	b.connections[name] = db

	return db, nil
}

func (b *databaseBackend) DatabaseConfig(s logical.Storage, name string) (*DatabaseConfig, error) {
	entry, err := s.Get(fmt.Sprintf("config/%s", name))
	if err != nil {
		return nil, fmt.Errorf("failed to read connection configuration with name: %s", name)
	}
	if entry == nil {
		return nil, fmt.Errorf("failed to find entry for connection with name: %s", name)
	}

	var config DatabaseConfig
	if err := entry.DecodeJSON(&config); err != nil {
		return nil, err
	}

	return &config, nil
}

func (b *databaseBackend) Role(s logical.Storage, n string) (*roleEntry, error) {
	entry, err := s.Get("role/" + n)
	if err != nil {
		return nil, err
	}
	if entry == nil {
		return nil, nil
	}

	var result roleEntry
	if err := entry.DecodeJSON(&result); err != nil {
		return nil, err
	}

	return &result, nil
}

func (b *databaseBackend) invalidate(key string) {
	b.Lock()
	defer b.Unlock()

	switch {
	case strings.HasPrefix(key, databaseConfigPath):
		name := strings.TrimPrefix(key, databaseConfigPath)
		b.clearConnection(name)
	}
}

// clearConnection closes the database connection and
// removes it from the b.connections map.
func (b *databaseBackend) clearConnection(name string) {
	db, ok := b.connections[name]
	if ok {
		db.Close()
		delete(b.connections, name)
	}
}

const backendHelp = `
The database backend supports using many different databases
as secret backends, including but not limited to:
cassandra, msslq, mysql, postgres

After mounting this backend, configure it using the endpoints within
the "database/config/" path.
`