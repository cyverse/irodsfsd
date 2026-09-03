package service

import (
	"encoding/json"
	"time"

	"github.com/cockroachdb/errors"
	irodsclient_fs "github.com/cyverse/go-irodsclient/fs"
	irodsclient_types "github.com/cyverse/go-irodsclient/irods/types"
	irodsfs_common_vpath "github.com/cyverse/irodsfs-common/irods/vpath"
	irodsfs_commons "github.com/cyverse/irodsfs/commons"
	"github.com/cyverse/irodsfsd/service/api"
	"google.golang.org/protobuf/types/known/durationpb"
)

const (
	clientServerNegotiationOff     = "off"
	clientServerNegotiationRequest = "request_server_negotiation"
)

func makeIRODSFSConfigJSON(config *api.MountConfig, mountID string, dataRootPath string) ([]byte, error) {
	if config == nil {
		return nil, errors.New("mount config is required")
	}
	irodsfsConfig := config.GetIrodsfs()
	if irodsfsConfig == nil {
		return nil, errors.New("irodsfs config is required")
	}
	if irodsfsConfig.Account == nil {
		return nil, errors.New("iRODS account is required")
	}

	input := irodsfs_commons.NewDefaultConfig()
	applyAccountConfig(input, irodsfsConfig.Account)

	input.MountPath = config.MountPath
	input.DataRootPath = dataRootPath
	if irodsfsConfig.ReadAheadMax != nil {
		input.ReadAheadMax = int(irodsfsConfig.GetReadAheadMax())
	}
	if irodsfsConfig.ReadWriteMax != nil {
		input.ReadWriteMax = int(irodsfsConfig.GetReadWriteMax())
	}
	input.Readonly = config.ReadOnly
	input.FuseOptions = append([]string(nil), config.MountOptions...)
	input.FuseOptions = append(input.FuseOptions, irodsfsConfig.FuseOptions...)
	if irodsfsConfig.Uid != nil {
		input.UID = int(irodsfsConfig.GetUid())
	}
	if irodsfsConfig.Gid != nil {
		input.GID = int(irodsfsConfig.GetGid())
	}
	input.SystemUser = irodsfsConfig.GetSystemUser()
	input.PoolEndpoint = irodsfsConfig.GetPoolEndpoint()
	input.Foreground = true
	input.Debug = irodsfsConfig.Debug
	input.InstanceID = mountID
	input.Description = irodsfsConfig.GetDescription()
	// irodsfs defaults to writing its own log file under DataRootPath,
	// duplicating (unredacted) what it already writes to stderr, which
	// irodsfsd separately captures and redacts into its own managed,
	// queryable mount log. "-" disables irodsfs's own file and keeps it to
	// stderr only, so there is exactly one, redacted copy of this log.
	input.LogPath = "-"

	input.PathMappings = make([]irodsfs_common_vpath.VPathMapping, 0, len(irodsfsConfig.PathMappings))
	for _, mapping := range irodsfsConfig.PathMappings {
		if mapping == nil {
			continue
		}
		input.PathMappings = append(input.PathMappings, irodsfs_common_vpath.VPathMapping{
			IRODSPath:           mapping.IrodsPath,
			MappingPath:         mapping.MappingPath,
			ResourceType:        irodsfs_common_vpath.VPathMappingResourceType(mapping.ResourceType),
			ReadOnly:            mapping.ReadOnly,
			CreateDir:           mapping.CreateDir,
			IgnoreNotExistError: mapping.IgnoreNotExistError,
		})
	}

	if err := applyConnectionConfig(&input.MetadataConnection, irodsfsConfig.MetadataConnection, "metadata_connection"); err != nil {
		return nil, err
	}
	if err := applyConnectionConfig(&input.IOConnection, irodsfsConfig.IoConnection, "io_connection"); err != nil {
		return nil, err
	}
	if err := applyCacheConfig(&input.Cache, irodsfsConfig.Cache); err != nil {
		return nil, err
	}

	configJSON, err := json.Marshal(input)
	if err != nil {
		return nil, errors.Wrap(err, "failed to marshal irodsfs configuration")
	}
	return configJSON, nil
}

func applyAccountConfig(input *irodsfs_commons.Config, account *api.Account) {
	if account.IrodsAuthenticationScheme != nil {
		input.AuthenticationScheme = account.GetIrodsAuthenticationScheme()
	}
	if account.IrodsClientServerNegotiation != nil {
		input.ClientServerNegotiation = clientServerNegotiationOff
		if account.GetIrodsClientServerNegotiation() {
			input.ClientServerNegotiation = clientServerNegotiationRequest
		}
	}
	if account.IrodsClientServerPolicy != nil {
		input.ClientServerPolicy = account.GetIrodsClientServerPolicy()
	}
	input.Host = account.IrodsHost
	if account.IrodsPort != 0 {
		input.Port = int(account.IrodsPort)
	}
	input.ZoneName = account.IrodsZoneName
	input.Username = account.IrodsUserName
	if account.IrodsClientZoneName != nil {
		input.ClientZoneName = account.GetIrodsClientZoneName()
	}
	if account.IrodsClientUserName != nil {
		input.ClientUsername = account.GetIrodsClientUserName()
	}
	if account.IrodsDefaultResource != nil {
		input.DefaultResource = account.GetIrodsDefaultResource()
	}
	if account.IrodsCwd != nil {
		input.CurrentWorkingDir = account.GetIrodsCwd()
	}
	if account.IrodsHome != nil {
		input.Home = account.GetIrodsHome()
	}
	if account.IrodsDefaultHashScheme != nil {
		input.DefaultHashScheme = account.GetIrodsDefaultHashScheme()
	}
	if account.IrodsMatchHashPolicy != nil {
		input.MatchHashPolicy = account.GetIrodsMatchHashPolicy()
	}
	if account.IrodsEncryptionAlgorithm != nil {
		input.EncryptionAlgorithm = account.GetIrodsEncryptionAlgorithm()
	}
	if account.IrodsEncryptionKeySize != nil {
		input.EncryptionKeySize = int(account.GetIrodsEncryptionKeySize())
	}
	if account.IrodsEncryptionSaltSize != nil {
		input.EncryptionSaltSize = int(account.GetIrodsEncryptionSaltSize())
	}
	if account.IrodsEncryptionNumHashRounds != nil {
		input.EncryptionNumHashRounds = int(account.GetIrodsEncryptionNumHashRounds())
	}
	if account.IrodsSslCaCertificateFile != nil {
		input.SSLCACertificateFile = account.GetIrodsSslCaCertificateFile()
	}
	if account.IrodsSslCaCertificatePath != nil {
		input.SSLCACertificatePath = account.GetIrodsSslCaCertificatePath()
	}
	if account.IrodsSslVerifyServer != nil {
		input.SSLVerifyServer = account.GetIrodsSslVerifyServer()
	}
	if account.IrodsSslCertificateChainFile != nil {
		input.SSLCertificateChainFile = account.GetIrodsSslCertificateChainFile()
	}
	if account.IrodsSslCertificateKeyFile != nil {
		input.SSLCertificateKeyFile = account.GetIrodsSslCertificateKeyFile()
	}
	if account.IrodsSslDhParamsFile != nil {
		input.SSLDHParamsFile = account.GetIrodsSslDhParamsFile()
	}
	if account.IrodsUserPassword != nil {
		input.Password = account.GetIrodsUserPassword()
	}
	if account.IrodsTicket != nil {
		input.Ticket = account.GetIrodsTicket()
	}
	if account.IrodsPamToken != nil {
		input.PAMToken = account.GetIrodsPamToken()
	}
	if account.IrodsPamTtl != nil {
		input.PAMTTL = int(account.GetIrodsPamTtl())
	}
	if account.IrodsSslServerName != nil {
		input.SSLServerName = account.GetIrodsSslServerName()
	}
}

func applyConnectionConfig(target *irodsclient_fs.ConnectionConfig, source *api.ConnectionConfig, fieldName string) error {
	if source == nil {
		return nil
	}

	if source.CreationTimeout != nil {
		duration, err := protobufDuration(source.CreationTimeout, fieldName+".creation_timeout")
		if err != nil {
			return err
		}
		target.CreationTimeout = irodsclient_types.Duration(duration)
	}
	if source.InitNumber != nil {
		target.InitNumber = int(source.GetInitNumber())
	}
	if source.MaxNumber != nil {
		target.MaxNumber = int(source.GetMaxNumber())
	}
	if source.MaxIdleNumber != nil {
		target.MaxIdleNumber = int(source.GetMaxIdleNumber())
	}
	if source.Lifespan != nil {
		duration, err := protobufDuration(source.Lifespan, fieldName+".lifespan")
		if err != nil {
			return err
		}
		target.Lifespan = irodsclient_types.Duration(duration)
	}
	if source.IdleTimeout != nil {
		duration, err := protobufDuration(source.IdleTimeout, fieldName+".idle_timeout")
		if err != nil {
			return err
		}
		target.IdleTimeout = irodsclient_types.Duration(duration)
	}
	if source.OperationTimeout != nil {
		duration, err := protobufDuration(source.OperationTimeout, fieldName+".operation_timeout")
		if err != nil {
			return err
		}
		target.OperationTimeout = irodsclient_types.Duration(duration)
	}
	if source.LongOperationTimeout != nil {
		duration, err := protobufDuration(source.LongOperationTimeout, fieldName+".long_operation_timeout")
		if err != nil {
			return err
		}
		target.LongOperationTimeout = irodsclient_types.Duration(duration)
	}
	if source.TcpBufferSize != nil {
		target.TcpBufferSize = int(source.GetTcpBufferSize())
	}
	if source.WaitConnection != nil {
		target.WaitConnection = source.GetWaitConnection()
	}
	return nil
}

func applyCacheConfig(target *irodsclient_fs.CacheConfig, source *api.CacheConfig) error {
	if source == nil {
		return nil
	}

	target.MetadataTimeoutSettings = make([]irodsclient_fs.MetadataCacheTimeoutSetting, 0, len(source.MetadataTimeoutSettings))
	for index, setting := range source.MetadataTimeoutSettings {
		if setting == nil {
			continue
		}
		duration, err := protobufDuration(setting.Timeout, "cache.metadata_timeout_settings.timeout")
		if err != nil {
			return errors.Wrapf(err, "invalid metadata timeout setting %d", index)
		}
		target.MetadataTimeoutSettings = append(target.MetadataTimeoutSettings, irodsclient_fs.MetadataCacheTimeoutSetting{
			Path:    setting.Path,
			Timeout: irodsclient_types.Duration(duration),
			Inherit: setting.GetInherit(),
		})
	}
	if source.StartNewTransaction != nil {
		target.StartNewTransaction = source.GetStartNewTransaction()
	}
	if source.Backend != nil {
		backend, err := makeCacheBackendConfig(source.Backend)
		if err != nil {
			return err
		}
		target.Backend = backend
	}
	return nil
}

func makeCacheBackendConfig(source *api.CacheBackendConfig) (*irodsclient_fs.CacheBackendConfig, error) {
	target := irodsclient_fs.NewDefaultCacheBackendConfig()
	if source.Type != "" {
		target.Type = irodsclient_fs.CacheBackendType(source.Type)
	}

	if source.Memory != nil {
		if source.Memory.CleanupInterval != nil {
			duration, err := protobufDuration(source.Memory.CleanupInterval, "cache.backend.memory.cleanup_interval")
			if err != nil {
				return nil, err
			}
			target.Memory.CleanupInterval = duration
		}
		if source.Memory.DefaultTtl != nil {
			duration, err := protobufDuration(source.Memory.DefaultTtl, "cache.backend.memory.default_ttl")
			if err != nil {
				return nil, err
			}
			target.Memory.DefaultTTL = duration
		}
	}
	if source.Ristretto != nil {
		if source.Ristretto.MaxEntries != nil {
			target.Ristretto.MaxEntries = source.Ristretto.GetMaxEntries()
		}
		if source.Ristretto.MaxCost != nil {
			target.Ristretto.MaxCost = source.Ristretto.GetMaxCost()
		}
		if source.Ristretto.BufferItems != nil {
			target.Ristretto.BufferItems = source.Ristretto.GetBufferItems()
		}
		if source.Ristretto.DefaultTtl != nil {
			duration, err := protobufDuration(source.Ristretto.DefaultTtl, "cache.backend.ristretto.default_ttl")
			if err != nil {
				return nil, err
			}
			target.Ristretto.DefaultTTL = duration
		}
	}
	if source.Redis != nil {
		if source.Redis.Address != nil {
			target.Redis.Address = source.Redis.GetAddress()
		}
		if source.Redis.Db != nil {
			target.Redis.DB = int(source.Redis.GetDb())
		}
		if source.Redis.Password != nil {
			target.Redis.Password = source.Redis.GetPassword()
		}
		if source.Redis.PoolSize != nil {
			target.Redis.PoolSize = int(source.Redis.GetPoolSize())
		}
		if source.Redis.KeyPrefix != nil {
			target.Redis.KeyPrefix = source.Redis.GetKeyPrefix()
		}
		if source.Redis.ConnectTimeout != nil {
			duration, err := protobufDuration(source.Redis.ConnectTimeout, "cache.backend.redis.connect_timeout")
			if err != nil {
				return nil, err
			}
			target.Redis.ConnectTimeout = duration
		}
		if source.Redis.CommandTimeout != nil {
			duration, err := protobufDuration(source.Redis.CommandTimeout, "cache.backend.redis.command_timeout")
			if err != nil {
				return nil, err
			}
			target.Redis.CommandTimeout = duration
		}
		if source.Redis.DefaultTtl != nil {
			duration, err := protobufDuration(source.Redis.DefaultTtl, "cache.backend.redis.default_ttl")
			if err != nil {
				return nil, err
			}
			target.Redis.DefaultTTL = duration
		}
		if source.Redis.EnableAccountIsolation != nil {
			target.Redis.EnableAccountIsolation = source.Redis.GetEnableAccountIsolation()
		}
	}
	return target, nil
}

func protobufDuration(value *durationpb.Duration, fieldName string) (time.Duration, error) {
	if value == nil {
		return 0, errors.Errorf("%s is required", fieldName)
	}
	if err := value.CheckValid(); err != nil {
		return 0, errors.Wrapf(err, "%s is invalid", fieldName)
	}
	return value.AsDuration(), nil
}
