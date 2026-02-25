package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log"
	"os"
	"sync/atomic"

	_ "modernc.org/sqlite"

	"github.com/nupi-ai/nupi/internal/config"
	storecrypto "github.com/nupi-ai/nupi/internal/config/store/crypto"
	"github.com/nupi-ai/nupi/internal/constants"
)

const (
	defaultBusyTimeout        = constants.Duration5Seconds
	defaultConnectionLifetime = 0 // unlimited
)

// keychainProberFunc is the function type for keychain availability probing.
type keychainProberFunc func(string, string) (storecrypto.SecretBackend, bool)

// probeKeychainPtr controls how Open() detects keychain availability.
// Defaults to probing the real OS keychain. Tests can override this
// via setKeychainProber to prevent non-deterministic real keychain access.
// Uses atomic.Pointer to allow safe concurrent access from Open() and
// test setup/teardown.
var probeKeychainPtr atomic.Pointer[keychainProberFunc]

func init() {
	fn := keychainProberFunc(defaultKeychainProber)
	probeKeychainPtr.Store(&fn)
}

func loadKeychainProber() keychainProberFunc {
	return *probeKeychainPtr.Load()
}

func defaultKeychainProber(instance, profile string) (storecrypto.SecretBackend, bool) {
	// NUPI_KEYCHAIN controls keychain behaviour:
	//   "off"   — disable keychain entirely, use AES-file only
	//   "force" — skip availability check, assume keychain works
	//   ""/"auto"/anything else — auto-detect via platform-safe pre-check
	env := os.Getenv("NUPI_KEYCHAIN")
	switch env {
	case "off":
		return nil, false
	case "force":
		kb := storecrypto.NewKeychainBackend(instance, profile)
		kb.SetForceAvailable()
		return kb, true
	default:
		if env != "" && env != "auto" {
			log.Printf("[Config] WARNING: unrecognized NUPI_KEYCHAIN=%q — valid values are \"off\", \"force\", \"auto\"; falling back to auto-detect", env)
		}
		kb := storecrypto.NewKeychainBackend(instance, profile)
		return kb, kb.Available()
	}
}

// setKeychainProber replaces the keychain probe function (for testing).
// Returns a cleanup function that restores the original prober.
func setKeychainProber(fn keychainProberFunc) func() {
	old := probeKeychainPtr.Load()
	probeKeychainPtr.Store(&fn)
	return func() { probeKeychainPtr.Store(old) }
}

// DisableKeychainForTesting disables real OS keychain probing so that
// store.Open() always uses the AES-file backend. Call from TestMain in
// any package whose tests transitively call store.Open().
// Returns a cleanup function that restores the default prober.
func DisableKeychainForTesting() func() {
	return setKeychainProber(func(_, _ string) (storecrypto.SecretBackend, bool) {
		return nil, false
	})
}

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
	secretBackend storecrypto.SecretBackend // abstracted secret storage (keychain or AES fallback)
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

// NotFound is a marker method enabling cross-package detection via interface
// (e.g., awareness package checks for not-found without importing config/store).
func (e NotFoundError) NotFound() bool { return true }

// IsNotFound returns true when err is (or wraps) a NotFoundError.
func IsNotFound(err error) bool {
	var target NotFoundError
	return errors.As(err, &target)
}

// DuplicateError indicates a record with the same unique key already exists.
type DuplicateError struct {
	Entity string
	Key    string
}

func (e DuplicateError) Error() string {
	if e.Key == "" {
		return fmt.Sprintf("%s already exists", e.Entity)
	}
	return fmt.Sprintf("%s %q already exists", e.Entity, e.Key)
}

// Duplicate is a marker method enabling cross-package detection via interface
// (e.g., awareness package checks for duplicates without importing config/store).
func (e DuplicateError) Duplicate() bool { return true }

// IsDuplicate returns true when err is (or wraps) a DuplicateError.
func IsDuplicate(err error) bool {
	var target DuplicateError
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

	ctx, cancel := context.WithTimeout(context.Background(), constants.Duration5Seconds)
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

	// Construct the secret backend: keychain (primary) + AES-file (fallback).
	var backend storecrypto.SecretBackend
	if encKey != nil {
		aesBackend := storecrypto.NewAESBackend(db, encKey, opts.InstanceName, opts.ProfileName)

		// probeKeychainFunc is replaceable for testing (see DisableKeychainForTesting).
		keychainBackend, keychainAvailable := loadKeychainProber()(opts.InstanceName, opts.ProfileName)
		if keychainAvailable && keychainBackend != nil {
			backend = storecrypto.NewFallbackBackend(keychainBackend, aesBackend)
			log.Printf("[Config] Secret backend: keychain (OS keyring) with AES-file fallback")
		} else {
			backend = aesBackend
			log.Printf("[Config] Secret backend: AES-file (OS keychain unavailable) — to enable OS keychain, unlock your keyring or set NUPI_KEYCHAIN=force")
		}
	} else if opts.ReadOnly {
		// No AES key in read-only mode — try keychain as a degraded backend.
		// Covers the edge case where .secrets.key was lost but the OS keychain
		// still has plaintext copies from dual-write.
		keychainBackend, keychainAvailable := loadKeychainProber()(opts.InstanceName, opts.ProfileName)
		if keychainAvailable && keychainBackend != nil {
			backend = keychainBackend
			log.Printf("[Config] Secret backend: keychain-only (no AES key — read-only degraded mode)")
		}
	}

	return &Store{
		db:            db,
		instanceName:  opts.InstanceName,
		profileName:   opts.ProfileName,
		dbPath:        dbPath,
		readOnly:      opts.ReadOnly,
		secretBackend: backend,
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

func (s *Store) ensureWritable(opName string) error {
	if s.readOnly {
		return fmt.Errorf("config: %s: store opened read-only", opName)
	}
	return nil
}

func (s *Store) withWriteTx(ctx context.Context, opName string, fn func(*sql.Tx) error) error {
	if err := s.ensureWritable(opName); err != nil {
		return err
	}
	return s.withTx(ctx, fn)
}
