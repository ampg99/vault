package database

import (
	"errors"
	"fmt"

	"github.com/fatih/structs"
	"github.com/hashicorp/vault/builtin/logical/database/dbplugin"
	"github.com/hashicorp/vault/helper/strutil"
	"github.com/hashicorp/vault/logical"
	"github.com/hashicorp/vault/logical/framework"
)

var (
	respErrEmptyPluginName = logical.ErrorResponse("empty plugin name")
	respErrEmptyName       = logical.ErrorResponse("empty name attribute given")
)

// DatabaseConfig is used by the Factory function to configure a Database
// object.
type DatabaseConfig struct {
	PluginName string `json:"plugin_name" structs:"plugin_name" mapstructure:"plugin_name"`
	// ConnectionDetails stores the database specific connection settings needed
	// by each database type.
	ConnectionDetails map[string]interface{} `json:"connection_details" structs:"connection_details" mapstructure:"connection_details"`
	AllowedRoles      []string               `json:"allowed_roles" structs:"allowed_roles" mapstructure:"allowed_roles"`
}

// pathResetConnection configures a path to reset a plugin.
func pathResetConnection(b *databaseBackend) *framework.Path {
	return &framework.Path{
		Pattern: fmt.Sprintf("reset/%s", framework.GenericNameRegex("name")),
		Fields: map[string]*framework.FieldSchema{
			"name": &framework.FieldSchema{
				Type:        framework.TypeString,
				Description: "Name of this database connection",
			},
		},

		Callbacks: map[logical.Operation]framework.OperationFunc{
			logical.UpdateOperation: b.pathConnectionReset(),
		},

		HelpSynopsis:    pathResetConnectionHelpSyn,
		HelpDescription: pathResetConnectionHelpDesc,
	}
}

// pathConnectionReset resets a plugin by closing the existing instance and
// creating a new one.
func (b *databaseBackend) pathConnectionReset() framework.OperationFunc {
	return func(req *logical.Request, data *framework.FieldData) (*logical.Response, error) {
		name := data.Get("name").(string)
		if name == "" {
			return respErrEmptyName, nil
		}

		// Grab the mutex lock
		b.Lock()
		defer b.Unlock()

		// Close plugin and delete the entry in the connections cache.
		b.clearConnection(name)

		// Execute plugin again, we don't need the object so throw away.
		_, err := b.getOrCreateDBObj(req.Storage, name)
		if err != nil {
			return nil, err
		}

		return nil, nil
	}
}

// pathConfigurePluginConnection returns a configured framework.Path setup to
// operate on plugins.
func pathConfigurePluginConnection(b *databaseBackend) *framework.Path {
	return &framework.Path{
		Pattern: fmt.Sprintf("config/%s", framework.GenericNameRegex("name")),
		Fields: map[string]*framework.FieldSchema{
			"name": &framework.FieldSchema{
				Type:        framework.TypeString,
				Description: "Name of this database connection",
			},

			"plugin_name": &framework.FieldSchema{
				Type: framework.TypeString,
				Description: `The name of a builtin or previously registered
				plugin known to vault. This endpoint will create an instance of
				that plugin type.`,
			},

			"verify_connection": &framework.FieldSchema{
				Type:    framework.TypeBool,
				Default: true,
				Description: `If true, the connection details are verified by
				actually connecting to the database. Defaults to true.`,
			},

			"allowed_roles": &framework.FieldSchema{
				Type: framework.TypeString,
				Description: `Comma separated list of the role names allowed to
				get creds from this database connection. If not set all roles
				are allowed.`,
			},
		},

		Callbacks: map[logical.Operation]framework.OperationFunc{
			logical.UpdateOperation: b.connectionWriteHandler(),
			logical.ReadOperation:   b.connectionReadHandler(),
			logical.DeleteOperation: b.connectionDeleteHandler(),
		},

		HelpSynopsis:    pathConfigConnectionHelpSyn,
		HelpDescription: pathConfigConnectionHelpDesc,
	}
}

// pathConnectionRead reads out the connection configuration
func (b *databaseBackend) connectionReadHandler() framework.OperationFunc {
	return func(req *logical.Request, data *framework.FieldData) (*logical.Response, error) {
		name := data.Get("name").(string)
		if name == "" {
			return respErrEmptyName, nil
		}

		entry, err := req.Storage.Get(fmt.Sprintf("config/%s", name))
		if err != nil {
			return nil, errors.New("failed to read connection configuration")
		}
		if entry == nil {
			return nil, nil
		}

		var config DatabaseConfig
		if err := entry.DecodeJSON(&config); err != nil {
			return nil, err
		}
		return &logical.Response{
			Data: structs.New(config).Map(),
		}, nil
	}
}

// connectionDeleteHandler deletes the connection configuration
func (b *databaseBackend) connectionDeleteHandler() framework.OperationFunc {
	return func(req *logical.Request, data *framework.FieldData) (*logical.Response, error) {
		name := data.Get("name").(string)
		if name == "" {
			return respErrEmptyName, nil
		}

		err := req.Storage.Delete(fmt.Sprintf("config/%s", name))
		if err != nil {
			return nil, errors.New("failed to delete connection configuration")
		}

		b.Lock()
		defer b.Unlock()

		if _, ok := b.connections[name]; ok {
			err = b.connections[name].Close()
			if err != nil {
				return nil, err
			}

			delete(b.connections, name)
		}

		return nil, nil
	}
}

// connectionWriteHandler returns a handler function for creating and updating
// both builtin and plugin database types.
func (b *databaseBackend) connectionWriteHandler() framework.OperationFunc {
	return func(req *logical.Request, data *framework.FieldData) (*logical.Response, error) {
		pluginName := data.Get("plugin_name").(string)
		if pluginName == "" {
			return respErrEmptyPluginName, nil
		}

		name := data.Get("name").(string)
		if name == "" {
			return respErrEmptyName, nil
		}

		verifyConnection := data.Get("verify_connection").(bool)

		// Pasrse and dedupe allowed roles from a comma separated string.
		allowedRolesRaw := data.Get("allowed_roles").(string)
		allowedRoles := strutil.ParseDedupAndSortStrings(allowedRolesRaw, ",")

		config := &DatabaseConfig{
			ConnectionDetails: data.Raw,
			PluginName:        pluginName,
			AllowedRoles:      allowedRoles,
		}

		db, err := dbplugin.PluginFactory(config.PluginName, b.System(), b.logger)
		if err != nil {
			return logical.ErrorResponse(fmt.Sprintf("error creating database object: %s", err)), nil
		}

		err = db.Initialize(config.ConnectionDetails, verifyConnection)
		if err != nil {
			db.Close()
			return logical.ErrorResponse(fmt.Sprintf("error creating database object: %s", err)), nil
		}

		// Grab the mutex lock
		b.Lock()
		defer b.Unlock()

		if _, ok := b.connections[name]; ok {
			// Close and remove the old connection
			err := b.connections[name].Close()
			if err != nil {
				db.Close()
				return nil, err
			}

			delete(b.connections, name)
		}

		// Save the new connection
		b.connections[name] = db

		// Store it
		entry, err := logical.StorageEntryJSON(fmt.Sprintf("config/%s", name), config)
		if err != nil {
			return nil, err
		}
		if err := req.Storage.Put(entry); err != nil {
			return nil, err
		}

		resp := &logical.Response{}
		resp.AddWarning("Read access to this endpoint should be controlled via ACLs as it will return the connection details as is, including passwords, if any.")

		return resp, nil
	}
}

const pathConfigConnectionHelpSyn = `
Configure connection details to a database plugin.
`

const pathConfigConnectionHelpDesc = `
This path configures the connection details used to connect to a particular
database. This path runs the provided plugin name and passes the configured
connection details to the plugin. See the documentation for the plugin specified
for a full list of accepted connection details. 

In addition to the database specific connection details, this endpoing also
accepts:

	* "plugin_name" (required) - The name of a builtin or previously registered
	   plugin known to vault. This endpoint will create an instance of that
	   plugin type.

	* "verify_connection" (default: true) - A boolean value denoting if the plugin should verify
	   it is able to connect to the database using the provided connection
       details.
`

const pathResetConnectionHelpSyn = `
Resets a database plugin.
`

const pathResetConnectionHelpDesc = `
This path resets the database connection by closing the existing database plugin
instance and running a new one.
`