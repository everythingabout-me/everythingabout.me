package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"text/tabwriter"

	"everythingabout.me/backend/internal/config"
	"everythingabout.me/backend/internal/connections"
	"everythingabout.me/backend/internal/store"
)

// runStorageConnection manages the storage_connections rows: adding a provider is
// an INSERT, so it is a command rather than a deployment edit.
//
//	eam storage-connection list
//	eam storage-connection add --name backup-nas --backend local --role backup \
//	    --local-content-root /srv/backup/content --local-system-root /srv/backup/system
func runStorageConnection(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf(`usage:
  eam storage-connection list
  eam storage-connection add --name NAME --backend cos|s3|local --role authority|backup [flags]

cos flags:   --cos-bucket --cos-region --cos-secret-id --cos-secret-key
s3 flags:    --s3-endpoint --s3-region --s3-bucket --s3-access-key-id --s3-secret-access-key [--s3-path-style] [--s3-content-prefix] [--s3-system-prefix]
local flags: --local-content-root --local-system-root [--local-case-sensitive]
common:      [--transfer-class] [--coordinated-writes=false] [--admission-file] [--spool-root] [--generation N]`)
	}
	switch args[0] {
	case "list":
		return runStorageConnectionList()
	case "add":
		return runStorageConnectionAdd(args[1:])
	default:
		return fmt.Errorf("unknown storage-connection subcommand %q", args[0])
	}
}

func runStorageConnectionList() error {
	ctx := context.Background()
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	db, err := store.Open(ctx, cfg.DatabaseURL)
	if err != nil {
		return err
	}
	defer db.Close()
	rows, err := connections.NewPostgresStore(db).Connections(ctx)
	if err != nil {
		return err
	}
	if len(rows) == 0 {
		fmt.Println("no storage connections: the server seeds one from configuration on first start")
		return nil
	}
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "NAME\tROLE\tBACKEND\tGENERATION\tENABLED\tLOCATION\tIDENTITY")
	for _, c := range rows {
		fmt.Fprintf(w, "%s\t%s\t%s\t%d\t%t\t%s\t%s\n", c.Name, c.Role, c.Backend, c.Generation, c.Enabled, connectionLocation(c), c.Identity[:12])
	}
	return w.Flush()
}

func connectionLocation(c connections.Connection) string {
	switch c.Backend {
	case "cos":
		return c.Config.COS.Bucket + "/" + c.Config.COS.Region
	case "s3":
		return c.Config.S3.Bucket + "/" + c.Config.S3.Region
	case "local":
		return c.Config.Local.ContentRoot
	}
	return ""
}

func runStorageConnectionAdd(args []string) error {
	fs := flag.NewFlagSet("storage-connection add", flag.ContinueOnError)
	name := fs.String("name", "", "connection name; owner bindings are written as name:digest")
	backend := fs.String("backend", "", "cos, s3 or local")
	role := fs.String("role", connections.RoleAuthority, "authority or backup")
	generation := fs.Int64("generation", 1, "generation; bump it when the location changes")
	transferClass := fs.String("transfer-class", "", "presigned-parts, resumable-session or relay")
	coordinated := fs.Bool("coordinated-writes", true, "require the coordinated object write protocol")
	admissionFile := fs.String("admission-file", "", "object admission evidence path (object backends)")
	spoolRoot := fs.String("spool-root", "", "object spool root (object backends)")
	cosBucket := fs.String("cos-bucket", "", "")
	cosRegion := fs.String("cos-region", "", "")
	cosSecretID := fs.String("cos-secret-id", "", "")
	cosSecretKey := fs.String("cos-secret-key", "", "")
	s3Endpoint := fs.String("s3-endpoint", "", "")
	s3Region := fs.String("s3-region", "", "")
	s3Bucket := fs.String("s3-bucket", "", "")
	s3AccessKey := fs.String("s3-access-key-id", "", "")
	s3SecretKey := fs.String("s3-secret-access-key", "", "")
	s3SessionToken := fs.String("s3-session-token", "", "")
	s3PathStyle := fs.Bool("s3-path-style", false, "")
	s3ContentPrefix := fs.String("s3-content-prefix", "", "defaults to drive/")
	s3SystemPrefix := fs.String("s3-system-prefix", "", "defaults to __eam_system__/")
	localContent := fs.String("local-content-root", "", "")
	localSystem := fs.String("local-system-root", "", "")
	localCase := fs.Bool("local-case-sensitive", false, "")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *name == "" || *backend == "" {
		return fmt.Errorf("--name and --backend are required: a connection is identified by its name and where it lives")
	}
	ctx := context.Background()
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	// Defaults follow the configured deployment so an operator does not restate the
	// write protocol for every new connection.
	c := connections.Connection{
		Name: *name, Backend: *backend, Role: *role, Generation: *generation, Enabled: true,
		Config: connections.Spec{
			CoordinatedWrites: *coordinated,
			TransferClass:     *transferClass,
			COS:               connections.COSSpec{Bucket: *cosBucket, Region: *cosRegion},
			S3: connections.S3Spec{
				Endpoint: *s3Endpoint, Region: *s3Region, Bucket: *s3Bucket,
				PathStyle: *s3PathStyle, ContentPrefix: *s3ContentPrefix, SystemPrefix: *s3SystemPrefix,
			},
			Local: connections.LocalSpec{ContentRoot: *localContent, SystemRoot: *localSystem, CaseSensitive: *localCase},
		},
		Secret: connections.Secret{
			COS: connections.COSCredential{SecretID: *cosSecretID, SecretKey: *cosSecretKey},
			S3:  connections.S3Credential{AccessKeyID: *s3AccessKey, SecretAccessKey: *s3SecretKey, SessionToken: *s3SessionToken},
		},
	}
	if c.Config.TransferClass == "" {
		c.Config.TransferClass = cfg.Storage.TransferClass
	}
	c.Config.ObjectAdmissionFile = *admissionFile
	c.Config.ObjectSpoolRoot = *spoolRoot
	if *backend != "local" {
		if c.Config.ObjectAdmissionFile == "" {
			c.Config.ObjectAdmissionFile = cfg.Storage.ObjectAdmissionFile
		}
		if c.Config.ObjectSpoolRoot == "" {
			c.Config.ObjectSpoolRoot = cfg.Storage.ObjectSpoolRoot
		}
	}
	identity, err := c.ComputeIdentity()
	if err != nil {
		return err
	}
	c.Identity = identity
	if err := c.Validate(); err != nil {
		return err
	}
	db, err := store.Open(ctx, cfg.DatabaseURL)
	if err != nil {
		return err
	}
	defer db.Close()
	if err := connections.NewPostgresStore(db).Insert(ctx, c); err != nil {
		return err
	}
	fmt.Printf("added storage connection %q (role %s, identity %s)\n", c.Name, c.Role, c.Identity[:12])
	fmt.Println("new connections are picked up on first use; changing which connection an owner is bound to is a rebinding and needs a restart")
	return nil
}
