package internal

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"

	"github.com/pkg/errors"
	"github.com/wal-g/wal-g/internal/compression"
	"github.com/wal-g/wal-g/internal/crypto"
	"github.com/wal-g/wal-g/internal/crypto/openpgp"
	"github.com/wal-g/wal-g/internal/compression/lz4"
	"github.com/wal-g/wal-g/internal/storages/storage"
	"github.com/wal-g/wal-g/internal/tracelog"
	"golang.org/x/time/rate"
)

const (
	DefaultDataBurstRateLimit = 8 * int64(DatabasePageSize)
	DefaultDataFolderPath     = "/tmp"
	WaleFileHost              = "file://localhost"
)

type UnconfiguredStorageError struct {
	error
}

func NewUnconfiguredStorageError(storagePrefixVariants []string) UnconfiguredStorageError {
	return UnconfiguredStorageError{errors.Errorf("No storage is configured now, please set one of following settings: %v", storagePrefixVariants)}
}

func (err UnconfiguredStorageError) Error() string {
	return fmt.Sprintf(tracelog.GetErrorFormatter(), err.error)
}

type UnknownCompressionMethodError struct {
	error
}

func NewUnknownCompressionMethodError() UnknownCompressionMethodError {
	return UnknownCompressionMethodError{errors.Errorf("Unknown compression method, supported methods are: %v", compression.CompressingAlgorithms)}
}

func (err UnknownCompressionMethodError) Error() string {
	return fmt.Sprintf(tracelog.GetErrorFormatter(), err.error)
}

// TODO : unit tests
func ConfigureLimiters() error {
	if diskLimitStr := GetSettingValue("WALG_DISK_RATE_LIMIT"); diskLimitStr != "" {
		diskLimit, err := strconv.ParseInt(diskLimitStr, 10, 64)
		if err != nil {
			return errors.Wrap(err, "failed to parse WALG_DISK_RATE_LIMIT")
		}
		DiskLimiter = rate.NewLimiter(rate.Limit(diskLimit), int(diskLimit+DefaultDataBurstRateLimit)) // Add 8 pages to possible bursts
	}

	if netLimitStr := GetSettingValue("WALG_NETWORK_RATE_LIMIT"); netLimitStr != "" {
		netLimit, err := strconv.ParseInt(netLimitStr, 10, 64)
		if err != nil {
			return errors.Wrap(err, "failed to parse WALG_NETWORK_RATE_LIMIT")
		}
		NetworkLimiter = rate.NewLimiter(rate.Limit(netLimit), int(netLimit+DefaultDataBurstRateLimit)) // Add 8 pages to possible bursts
	}
	return nil
}

// TODO : unit tests
func ConfigureFolder() (storage.Folder, error) {
	skippedPrefixes := make([]string, 0)
	for _, adapter := range StorageAdapters {
		prefix := GetSettingValue(adapter.prefixName)
		if prefix == "" {
			skippedPrefixes = append(skippedPrefixes, adapter.prefixName)
			continue
		}
		if adapter.prefixPreprocessor != nil {
			prefix = adapter.prefixPreprocessor(prefix)
		}

		settings, err := adapter.loadSettings()
		if err != nil {
			return nil, err
		}
		return adapter.configureFolder(prefix, settings)
	}
	return nil, NewUnconfiguredStorageError(skippedPrefixes)
}

// TODO : unit tests
func getDataFolderPath() string {
	pgdata, ok := LookupValue("PGDATA")
	var dataFolderPath string
	if !ok {
		dataFolderPath = DefaultDataFolderPath
	} else {
		dataFolderPath = filepath.Join(pgdata, "pg_wal")
		if _, err := os.Stat(dataFolderPath); err != nil {
			dataFolderPath = filepath.Join(pgdata, "pg_xlog")
			if _, err := os.Stat(dataFolderPath); err != nil {
				dataFolderPath = DefaultDataFolderPath
			}
		}
	}
	dataFolderPath = filepath.Join(dataFolderPath, "walg_data")
	return dataFolderPath
}

func ConfigurePreventWalOverwrite() (preventWalOverwrite bool, err error) {
	err = nil
	preventWalOverwrite = false
	preventWalOverwriteStr := GetSettingValue("WALG_PREVENT_WAL_OVERWRITE")

	if preventWalOverwriteStr != "" {
		preventWalOverwrite, err = strconv.ParseBool(preventWalOverwriteStr)
		if err != nil {
			return false, errors.Wrap(err, "failed to parse WALG_PREVENT_WAL_OVERWRITE")
		}
	}

	return preventWalOverwrite, nil
}

// TODO : unit tests
func configureWalDeltaUsage() (useWalDelta bool, deltaDataFolder DataFolder, err error) {
	if useWalDeltaStr, ok := LookupValue("WALG_USE_WAL_DELTA"); ok {
		useWalDelta, err = strconv.ParseBool(useWalDeltaStr)
		if err != nil {
			return false, nil, errors.Wrapf(err, "failed to parse WALG_USE_WAL_DELTA")
		}
	}
	if !useWalDelta {
		return
	}
	dataFolderPath := getDataFolderPath()
	deltaDataFolder, err = NewDiskDataFolder(dataFolderPath)
	if err != nil {
		useWalDelta = false
		tracelog.WarningLogger.Printf("can't use wal delta feature because can't open delta data folder '%s'"+
			" due to error: '%v'\n", dataFolderPath, err)
		err = nil
	}
	return
}

// TODO : unit tests
func configureCompressor() (compression.Compressor, error) {
	compressionMethod := GetSettingValue("WALG_COMPRESSION_METHOD")
	if compressionMethod == "" {
		compressionMethod = lz4.AlgorithmName
	}
	if _, ok := compression.Compressors[compressionMethod]; !ok {
		return nil, NewUnknownCompressionMethodError()
	}
	return compression.Compressors[compressionMethod], nil
}

// TODO : unit tests
func ConfigureLogging() error {
	logLevel, ok := LookupValue("WALG_LOG_LEVEL")
	if ok {
		return tracelog.UpdateLogLevel(logLevel)
	}
	return nil
}

// ConfigureUploader connects to storage and creates an uploader. It makes sure
// that a valid session has started; if invalid, returns AWS error
// and `<nil>` values.
func ConfigureUploader() (uploader *Uploader, err error) {
	folder, err := ConfigureFolder()
	if err != nil {
		return nil, errors.Wrap(err, "failed to configure folder")
	}

	compressor, err := configureCompressor()
	if err != nil {
		return nil, errors.Wrap(err, "failed to configure compression")
	}

	useWalDelta, deltaDataFolder, err := configureWalDeltaUsage()
	if err != nil {
		return nil, errors.Wrap(err, "failed to configure WAL Delta usage")
	}

	uploader = NewUploader(compressor, folder, deltaDataFolder, useWalDelta)

	return uploader, err
}

// ConfigureCrypter uses environment variables to create and configure a crypter.
// In case no configuration in environment variables found, return `<nil>` value.
func ConfigureCrypter() crypto.Crypter {
	passphrase, isExist := config.LookupValue("WALG_PGP_KEY_PASSPHRASE")

	if !isExist {
		return nil
	}

	// key can be either private (for download) or public (for upload)
	armoredKey, isKeyExist := LookupValue("WALG_PGP_KEY")

	if isKeyExist {
		return openpgp.CrypterFromArmoredKey(armoredKey, passphrase)
	}

	// key can be either private (for download) or public (for upload)
	armoredKeyPath, isPathExist := LookupValue("WALG_PGP_KEY_PATH")

	if isPathExist {
		return openpgp.CrypterFromArmoredKeyPath(armoredKeyPath, passphrase)
	}

	keyRingID := GetSettingValue("WALE_GPG_KEY_ID")

	if keyRingID != "" {
		return openpgp.CrypterFromKeyRingID(keyRingID, passphrase)
	}

	return nil
}