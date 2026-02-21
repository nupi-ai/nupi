package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log"
	"time"

	_ "modernc.org/sqlite"

	"github.com/nupi-ai/nupi/internal/config"
	storecrypto "github.com/nupi-ai/nupi/internal/config/store/crypto"
)

const (
	defaultBusyTimeout        = 5 * time.Second
	defaultConnectionLifetime = 0 // unlimited
)

// Options describes parameters for opening a configuration store.
type Options struct {
	InstanceName string // Logical instance name (defaults to config.DefaultInstance)
	ProfileName  string // Profile within instance (defaults to config.DefaultProfile)
	DBPath       string // Optional override for config.db path (primarily for tests)
	ReadOnly     bool   // Open database in read-only mode
}

// Store provides access to the configuration database.
type Store struct {
	db            *sql.DB
	instanceName  string
	profileName   string
	dbPath        string
	readOnly      bool
	encryptionKey []byte // AES-256 key for encrypting security settings
}

// NotFoundError indicates a requested record does not exist.
type NotFoundError struct {
	Entity string
	Key    string
}

func (e NotFoundError) Error() string {
	if e.Key == "" {
		return fmt.Sprintf("%s not found", e.Entity)
	}
	return fmt.Sprintf("%s %s not found", e.Entity, e.Key)
}

// IsNotFound returns true when err is (or wraps) a NotFoundError.
func IsNotFound(err error) bool {
	var target NotFoundError
	return errors.As(err, &target)
}

// Open initialises the configuration store for the given instance/profile.
func Open(opts Options) (*Store, error) {
	if opts.InstanceName == "" {
		opts.InstanceName = config.DefaultInstance
	}
	if opts.ProfileName == "" {
		opts.ProfileName = config.DefaultProfile
	}

	instancePaths, err := config.EnsureInstanceDirs(opts.InstanceName)
	if err != nil {
		return nil, fmt.Errorf("config: ensure instance directories: %w", err)
	}

	if _, err := config.EnsureProfileDirs(opts.InstanceName, opts.ProfileName); err != nil {
		return nil, fmt.Errorf("config: ensure profile directories: %w", err)
	}

	dbPath := opts.DBPath
	if dbPath == "" {
		dbPath = instancePaths.ConfigDB
	}

	dsn := dbPath
	if opts.ReadOnly {
		dsn = fmt.Sprintf("file:%s?mode=ro", dbPath)
	}

	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("config: open sqlite store: %w", err)
	}

	db.SetMaxOpenConns(1)
	db.SetConnMaxLifetime(defaultConnectionLifetime)
	db.SetConnMaxIdleTime(defaultConnectionLifetime)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := applyPragmas(ctx, db, opts.ReadOnly); err != nil {
		db.Close()
		return nil, err
	}

	if err := applySchema(ctx, db); err != nil {
		db.Close()
		return nil, err
	}

	if !opts.ReadOnly {
		if err := applyMigrations(ctx, db); err != nil {
			db.Close()
			return nil, err
		}
	}

	if !opts.ReadOnly {
		if err := seedDefaults(ctx, db, opts.InstanceName, opts.ProfileName); err != nil {
			db.Close()
			return nil, err
		}
	}

	// Load or create encryption key for security settings.
	//
	// Safety invariant: a new key is only created when the DB contains no
	// enc:v1: values. If the key file is missing but encrypted rows already
	// exist, Open fails fast to prevent permanent data loss (old secrets
	// would become undecryptable with a freshly-generated key).
	keyPath := storecrypto.KeyPath(dbPath)
	var encKey []byte
	if !opts.ReadOnly {
		encKey, err = storecrypto.LoadKey(keyPath)
		if err != nil {
			db.Close()
			return nil, err
		}
		if encKey == nil {
			// Key file does not exist. Safe to create only if the DB has
			// no previously-encrypted values.
			hasEnc, checkErr := storecrypto.HasEncryptedValues(ctx, db)
			if checkErr != nil {
				db.Close()
				return nil, checkErr
			}
			if hasEnc {
				db.Close()
				return nil, fmt.Errorf("config: encryption key %s is missing but the database already contains encrypted values — refusing to create a new key to prevent data loss; restore the original key file or remove the encrypted rows manually", keyPath)
			}
			encKey, err = storecrypto.CreateKey(keyPath)
			if err != nil {
				db.Close()
				return nil, err
			}
		}
	} else {
		// Read-only mode: only load existing key, never create.
		// A missing key is normal (first open); a corrupt key is logged.
		var keyErr error
		encKey, keyErr = storecrypto.LoadKey(keyPath)
		if keyErr != nil {
			log.Printf("[Config] WARNING: failed to load encryption key (read-only): %v — encrypted settings will be unreadable", keyErr)
		}
	}

	// Backfill: re-encrypt any legacy plaintext secrets left over from
	// before encryption was introduced. This ensures full encryption at rest.
	if !opts.ReadOnly && encKey != nil {
		if migrated, err := storecrypto.MigratePlaintext(ctx, db, encKey); err != nil {
			db.Close()
			return nil, fmt.Errorf("config: migrate plaintext secrets: %w", err)
		} else if migrated > 0 {
			log.Printf("[Config] Migrated %d plaintext secret(s) to encrypted storage", migrated)
		}
	}

	return &Store{
		db:            db,
		instanceName:  opts.InstanceName,
		profileName:   opts.ProfileName,
		dbPath:        dbPath,
		readOnly:      opts.ReadOnly,
		encryptionKey: encKey,
	}, nil
}

// Close finalises the underlying database connection.
func (s *Store) Close() error {
	if s == nil || s.db == nil {
		return nil
	}
	return s.db.Close()
}

// DB exposes the underlying sql.DB handle for internal usage.
func (s *Store) DB() *sql.DB {
	return s.db
}

// InstanceName returns the logical instance associated with the store.
func (s *Store) InstanceName() string {
	return s.instanceName
}

// ProfileName returns the active profile associated with the store.
func (s *Store) ProfileName() string {
	return s.profileName
}

// Path returns the filesystem path of the backing database.
func (s *Store) Path() string {
	return s.dbPath
}

func (s *Store) withTx(ctx context.Context, fn func(*sql.Tx) error) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}

	if err := fn(tx); err != nil {
		if rbErr := tx.Rollback(); rbErr != nil {
			return fmt.Errorf("config: rollback failed after %v: %w", err, rbErr)
		}
		return err
	}

	return tx.Commit()
}
