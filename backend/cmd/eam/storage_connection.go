package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"text/tabwriter"

	"everythingabout.me/backend/internal/config"
	"everythingabout.me/backend/internal/connections"
	"everythingabout.me/backend/internal/storagebackend"
	"everythingabout.me/backend/internal/store"
)

// runStorageConnection manages the storage_connections rows: adding a provider is
// an INSERT, so it is a command rather than a deployment edit.
//
//	eam storage-connection list [--owner email-or-UUID]
//	eam storage-connection add --name backup-nas --backend local --role backup \
//	   --local-content-root /srv/backup/content --local-system-root /srv/backup/system
//	eam storage-connection add --owner you@example.com --name my-cos --backend cos --role backup ...
//	eam storage-connection probe --name nas
//
// Without --owner a connection is the deployment default that every account may
// use. With --owner it belongs to that account alone; such a connection may only
// be a COS bucket, because a local root would hand an account arbitrary host paths
// and a custom S3 endpoint would turn this process into an outbound request proxy.
func runStorageConnection(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf(`usage:
  eam storage-connection list [--owner email-or-UUID]
  eam storage-connection add [--owner email-or-UUID] --name NAME --backend cos|s3|local --role authority|backup [flags]
  eam storage-connection probe [--owner email-or-UUID] --name NAME

cos flags:   --cos-bucket --cos-region --cos-secret-id --cos-secret-key
s3 flags:    --s3-endpoint --s3-region --s3-bucket --s3-access-key-id --s3-secret-access-key [--s3-path-style] [--s3-content-prefix] [--s3-system-prefix]
local flags: --local-content-root --local-system-root [--local-case-sensitive]
root:        [--root-at-bucket]  (with --owner: the tree is the bucket itself, not a prefix in it)
common:      [--transfer-class] [--coordinated-writes=false] [--admission-file] [--spool-root] [--generation N]`)
	}
	switch args[0] {
	case "list":
		return runStorageConnectionList(args[1:])
	case "add":
		return runStorageConnectionAdd(args[1:])
	case "probe":
		return runStorageConnectionProbe(args[1:])
	default:
		return fmt.Errorf("unknown storage-connection subcommand %q", args[0])
	}
}

func runStorageConnectionList(args []string) error {
	fs := flag.NewFlagSet("storage-connection list", flag.ContinueOnError)
	owner := fs.String("owner", "", "scope to one account (email or UUID) plus the deployment default")
	if err := fs.Parse(args); err != nil {
		return err
	}
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
	connStore := connections.NewPostgresStore(db)
	var rows []connections.Connection
	if *owner != "" {
		ownerID, err := resolveOwnerID(ctx, db, *owner)
		if err != nil {
			return err
		}
		rows, err = connStore.Visible(ctx, ownerID)
		if err != nil {
			return err
		}
	} else {
		rows, err = connStore.Connections(ctx)
	}
	if err != nil {
		return err
	}
	if len(rows) == 0 {
		fmt.Println("no storage connections: the server seeds one from configuration on first start")
		return nil
	}
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "NAME\tROLE\tBACKEND\tSCOPE\tGENERATION\tENABLED\tLOCATION\tIDENTITY")
	for _, c := range rows {
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%d\t%t\t%s\t%s\n", c.Name, c.Role, c.Backend, connectionScope(c), c.Generation, c.Enabled, connectionLocation(c), c.Identity[:12])
	}
	return w.Flush()
}

// connectionScope says whose tree a row holds, because that decides where the tree
// starts: one account's tree is the prefix itself, a shared row namespaces every
// account under it.
func connectionScope(c connections.Connection) string {
	if c.OwnerID == "" {
		return "shared"
	}
	return "owner"
}

// reservedSystemName is the top-level folder a bucket-rooted tree must never contain
// as user content, because the service's private prefix lives there.
func reservedSystemName(c connections.Connection) string {
	switch c.Backend {
	case "cos":
		return "__eam_system__/"
	case "s3":
		if pref := c.Config.S3.SystemPrefix; pref != "" {
			return pref
		}
		return "__eam_system__/"
	}
	return ""
}

// objectPrefix is the configured native prefix, before any account segment. It is
// what the operator chose as the location; empty means a backend that has none.
func objectPrefix(c connections.Connection) string {
	switch c.Backend {
	case "cos":
		return "drive/"
	case "s3":
		if c.Config.S3.ContentPrefix != "" {
			return c.Config.S3.ContentPrefix
		}
		return "drive/"
	}
	return ""
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
	owner := fs.String("owner", "", "email or UUID: give the connection to one account instead of making it the deployment default")
	name := fs.String("name", "", "connection name; owner bindings are written as name:digest")
	backend := fs.String("backend", "", "cos, s3 or local")
	role := fs.String("role", connections.RoleAuthority, "authority or backup")
	generation := fs.Int64("generation", 1, "generation; bump it when the location changes")
	transferClass := fs.String("transfer-class", "", "presigned-parts, resumable-session or relay")
	rootAtBucket := fs.Bool("root-at-bucket", false, "put this account's tree at the bucket root instead of under a prefix (needs --owner)")
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
	if *rootAtBucket && *owner == "" {
		return fmt.Errorf("--root-at-bucket puts one account's tree at the bucket root, so it needs --owner: a shared connection would put every account's tree in the same place")
	}
	if *rootAtBucket && *s3ContentPrefix != "" {
		return fmt.Errorf("--root-at-bucket and --s3-content-prefix name two different roots; the tree is either the bucket or a prefix in it")
	}
	if err := connections.ValidateName(*name); err != nil {
		return err
	}
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
	// Defaults follow the configured deployment so an operator does not restate the
	// write protocol for every new connection.
	var ownerID string
	if *owner != "" {
		ownerID, err = resolveOwnerID(ctx, db, *owner)
		if err != nil {
			return err
		}
	}
	c := connections.Connection{
		OwnerID: ownerID, Name: *name, Backend: *backend, Role: *role, Generation: *generation, Enabled: true,
		Config: connections.Spec{
			RootAtBucket:      *rootAtBucket,
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
	if *backend != "local" {
		c.Config.ObjectAdmissionFile, c.Config.ObjectSpoolRoot, err = connections.ObjectPaths(cfg.Storage, *name, *backend, *admissionFile, *spoolRoot)
		if err != nil {
			return err
		}
		if *spoolRoot == "" {
			// The path is derived under a spool root the deployment already writes
			// to, so creating this child cannot hide an unmounted volume: the parent
			// exists because the deployment uses it.
			if err := os.MkdirAll(c.Config.ObjectSpoolRoot, 0o700); err != nil {
				return fmt.Errorf("create derived spool root %s: %w", c.Config.ObjectSpoolRoot, err)
			}
		} else if info, statErr := os.Stat(c.Config.ObjectSpoolRoot); statErr != nil || !info.IsDir() {
			// An explicit path is never created: a missing one is usually a volume
			// that failed to mount, and silently making the directory would put this
			// connection's temporary bytes on the wrong disk.
			return fmt.Errorf("--spool-root %s must be an existing directory", c.Config.ObjectSpoolRoot)
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
	if err := connections.NewPostgresStore(db).Insert(ctx, c); err != nil {
		return err
	}
	fmt.Printf("added storage connection %q (role %s, scope %s, identity %s)\n", c.Name, c.Role, connectionScope(c), c.Identity[:12])
	if c.Config.RootAtBucket {
		fmt.Printf("  this account's tree is the bucket root; %s is reserved for the service and refused as a user name\n", reservedSystemName(c))
	} else if root := objectPrefix(c); root != "" {
		if c.OwnerID != "" {
			fmt.Printf("  this account's tree is %s (no account segment)\n", root)
		} else {
			fmt.Printf("  each account's tree is %s<owner-uuid>/\n", root)
		}
	}
	if c.Backend != "local" {
		fmt.Printf("  admission evidence: %s\n  spool root: %s\n", c.Config.ObjectAdmissionFile, c.Config.ObjectSpoolRoot)
		fmt.Printf("  object writes stay unproven until: eam storage-connection probe --name %s\n", c.Name)
	}
	fmt.Println("new connections are picked up on first use; changing which connection an owner is bound to is a rebinding and needs a restart")
	return nil
}

// runStorageConnectionProbe measures one connection and records the evidence its
// driver needs before it may publish.
func runStorageConnectionProbe(args []string) error {
	fs := flag.NewFlagSet("storage-connection probe", flag.ContinueOnError)
	owner := fs.String("owner", "", "scope to one account (email or UUID); omit for a deployment default row")
	name := fs.String("name", "", "connection name to measure")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *name == "" {
		return fmt.Errorf("--name is required: a probe measures one named connection")
	}
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
	var ownerID string
	if *owner != "" {
		ownerID, err = resolveOwnerID(ctx, db, *owner)
		if err != nil {
			return err
		}
	}
	c, ok, err := connections.NewPostgresStore(db).ByName(ctx, ownerID, *name)
	if err != nil {
		return err
	}
	if !ok {
		scope := "the deployment default"
		if *owner != "" {
			scope = "owner " + *owner
		}
		return fmt.Errorf("storage connection %q does not exist for %s; add it first with `eam storage-connection add --name %s ...`", *name, scope, *name)
	}
	effective, err := c.Apply(*cfg)
	if err != nil {
		return err
	}
	admission, err := storagebackend.ProbeConnection(ctx, &effective)
	if err != nil {
		return err
	}
	fmt.Printf("probed storage connection %q: %s bucket %q region %q\n", c.Name, admission.Endpoint, admission.Bucket, admission.Region)
	fmt.Printf("  evidence: %s\n  transcript: %s\n", effective.Storage.ObjectAdmissionFile, admission.ProbeReference)
	fmt.Printf("  conditional create: %t  atomic publish: %t  multipart: %t  form post: %t\n",
		admission.ConditionalCreate, admission.AtomicPublish, admission.MultipartUploads, admission.FormPost)
	return nil
}
