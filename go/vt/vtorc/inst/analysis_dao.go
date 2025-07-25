/*
   Copyright 2015 Shlomi Noach, courtesy Booking.com

   Licensed under the Apache License, Version 2.0 (the "License");
   you may not use this file except in compliance with the License.
   You may obtain a copy of the License at

       http://www.apache.org/licenses/LICENSE-2.0

   Unless required by applicable law or agreed to in writing, software
   distributed under the License is distributed on an "AS IS" BASIS,
   WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
   See the License for the specific language governing permissions and
   limitations under the License.
*/

package inst

import (
	"fmt"
	"math"
	"time"

	"github.com/patrickmn/go-cache"
	"google.golang.org/protobuf/encoding/prototext"

	"vitess.io/vitess/go/stats"
	"vitess.io/vitess/go/vt/external/golib/sqlutils"
	"vitess.io/vitess/go/vt/log"
	topodatapb "vitess.io/vitess/go/vt/proto/topodata"
	"vitess.io/vitess/go/vt/topo"
	"vitess.io/vitess/go/vt/topo/topoproto"
	"vitess.io/vitess/go/vt/vtctl/reparentutil/policy"
	"vitess.io/vitess/go/vt/vtorc/config"
	"vitess.io/vitess/go/vt/vtorc/db"
	"vitess.io/vitess/go/vt/vtorc/util"
)

var analysisChangeWriteCounter = stats.NewCounter("AnalysisChangeWrite", "Number of times analysis has changed")

var recentInstantAnalysis *cache.Cache

func init() {
	go initializeAnalysisDaoPostConfiguration()
}

func initializeAnalysisDaoPostConfiguration() {
	config.WaitForConfigurationToBeLoaded()

	recentInstantAnalysis = cache.New(config.GetRecoveryPollDuration()*2, time.Second)
}

type clusterAnalysis struct {
	hasShardWideAction bool
	totalTablets       int
	primaryAlias       string
	durability         policy.Durabler
}

// GetReplicationAnalysis will check for replication problems (dead primary; unreachable primary; etc)
func GetReplicationAnalysis(keyspace string, shard string, hints *ReplicationAnalysisHints) ([]*ReplicationAnalysis, error) {
	var result []*ReplicationAnalysis
	appendAnalysis := func(analysis *ReplicationAnalysis) {
		if analysis.Analysis == NoProblem && len(analysis.StructureAnalysis) == 0 {
			return
		}
		result = append(result, analysis)
	}

	// TODO(sougou); deprecate ReduceReplicationAnalysisCount
	args := sqlutils.Args(config.GetReasonableReplicationLagSeconds(), ValidSecondsFromSeenToLastAttemptedCheck(), config.GetReasonableReplicationLagSeconds(), keyspace, shard)
	query := `SELECT
		vitess_tablet.info AS tablet_info,
		vitess_tablet.tablet_type,
		vitess_tablet.primary_timestamp,
		vitess_tablet.shard AS shard,
		vitess_keyspace.keyspace AS keyspace,
		vitess_keyspace.keyspace_type AS keyspace_type,
		vitess_keyspace.durability_policy AS durability_policy,
		vitess_shard.primary_timestamp AS shard_primary_term_timestamp,
		primary_instance.read_only AS read_only,
		MIN(primary_instance.gtid_errant) AS gtid_errant,
		MIN(primary_instance.alias) IS NULL AS is_invalid,
		MIN(primary_instance.binary_log_file) AS binary_log_file,
		MIN(primary_instance.binary_log_pos) AS binary_log_pos,
		MIN(primary_instance.replica_net_timeout) AS replica_net_timeout,
		MIN(primary_instance.heartbeat_interval) AS heartbeat_interval,
		MIN(primary_tablet.info) AS primary_tablet_info,
		MIN(
			IFNULL(
				primary_instance.binary_log_file = database_instance_stale_binlog_coordinates.binary_log_file
				AND primary_instance.binary_log_pos = database_instance_stale_binlog_coordinates.binary_log_pos
				AND database_instance_stale_binlog_coordinates.first_seen < DATETIME('now', PRINTF('-%d SECOND', ?)),
				0
			)
		) AS is_stale_binlog_coordinates,
		MIN(
			primary_instance.last_checked <= primary_instance.last_seen
			and primary_instance.last_attempted_check <= DATETIME(primary_instance.last_seen, PRINTF('+%d SECOND', ?))
		) = 1 AS is_last_check_valid,
		/* To be considered a primary, traditional async replication must not be present/valid AND the host should either */
		/* not be a replication group member OR be the primary of the replication group */
		MIN(primary_instance.last_check_partial_success) as last_check_partial_success,
		MIN(
			(
				primary_instance.source_host IN ('', '_')
				OR primary_instance.source_port = 0
				OR substr(primary_instance.source_host, 1, 2) = '//'
			)
		) AS is_primary,
		MIN(primary_instance.gtid_mode) AS gtid_mode,
		COUNT(replica_instance.server_id) AS count_replicas,
		IFNULL(
			SUM(
				replica_instance.last_checked <= replica_instance.last_seen
			),
			0
		) AS count_valid_replicas,
		IFNULL(
			SUM(
				replica_instance.last_checked <= replica_instance.last_seen
				AND replica_instance.replica_io_running != 0
				AND replica_instance.replica_sql_running != 0
			),
			0
		) AS count_valid_replicating_replicas,
		IFNULL(
			SUM(
				replica_instance.last_checked <= replica_instance.last_seen
				AND replica_instance.replica_io_running = 0
				AND replica_instance.last_io_error like '%%error %%connecting to master%%'
				AND replica_instance.replica_sql_running = 1
			),
			0
		) AS count_replicas_failing_to_connect_to_primary,
		MIN(
			primary_instance.replica_sql_running = 0
			OR primary_instance.replica_io_running = 0
		) AS replication_stopped,
		MIN(
			primary_instance.supports_oracle_gtid
		) AS supports_oracle_gtid,
		MIN(
			primary_instance.semi_sync_primary_enabled
		) AS semi_sync_primary_enabled,
		MIN(
			primary_instance.semi_sync_primary_wait_for_replica_count
		) AS semi_sync_primary_wait_for_replica_count,
		MIN(
			primary_instance.semi_sync_primary_clients
		) AS semi_sync_primary_clients,
		MIN(
			primary_instance.semi_sync_primary_status
		) AS semi_sync_primary_status,
		MIN(
			primary_instance.semi_sync_blocked
		) AS semi_sync_blocked,
		MIN(
			primary_instance.semi_sync_replica_enabled
		) AS semi_sync_replica_enabled,
		MIN(
			primary_instance.tablet_type
		) AS current_tablet_type,
		SUM(replica_instance.oracle_gtid) AS count_oracle_gtid_replicas,
		IFNULL(
			SUM(
				replica_instance.last_checked <= replica_instance.last_seen
				AND replica_instance.oracle_gtid != 0
			),
			0
		) AS count_valid_oracle_gtid_replicas,
		SUM(
			replica_instance.binlog_server
		) AS count_binlog_server_replicas,
		IFNULL(
			SUM(
				replica_instance.last_checked <= replica_instance.last_seen
				AND replica_instance.binlog_server != 0
			),
			0
		) AS count_valid_binlog_server_replicas,
		SUM(
			replica_instance.semi_sync_replica_enabled
		) AS count_semi_sync_replicas,
		IFNULL(
			SUM(
				replica_instance.last_checked <= replica_instance.last_seen
				AND replica_instance.semi_sync_replica_enabled != 0
			),
			0
		) AS count_valid_semi_sync_replicas,
		IFNULL(
			SUM(
				replica_instance.log_bin
				AND replica_instance.log_replica_updates
			),
			0
		) AS count_logging_replicas,
		IFNULL(
			SUM(
				replica_instance.log_bin
				AND replica_instance.log_replica_updates
				AND replica_instance.binlog_format = 'STATEMENT'
			),
			0
		) AS count_statement_based_logging_replicas,
		IFNULL(
			SUM(
				replica_instance.log_bin
				AND replica_instance.log_replica_updates
				AND replica_instance.binlog_format = 'MIXED'
			),
			0
		) AS count_mixed_based_logging_replicas,
		IFNULL(
			SUM(
				replica_instance.log_bin
				AND replica_instance.log_replica_updates
				AND replica_instance.binlog_format = 'ROW'
			),
			0
		) AS count_row_based_logging_replicas,
		IFNULL(
			SUM(replica_instance.sql_delay > 0),
			0
		) AS count_delayed_replicas,
		IFNULL(
			SUM(replica_instance.replica_lag_seconds > ?),
			0
		) AS count_lagging_replicas,
		IFNULL(MIN(replica_instance.gtid_mode), '') AS min_replica_gtid_mode,
		IFNULL(MAX(replica_instance.gtid_mode), '') AS max_replica_gtid_mode,
		IFNULL(
			MAX(
				replica_instance.gtid_errant
			),
			''
		) AS max_replica_gtid_errant,
		COUNT(
			DISTINCT case when replica_instance.log_bin
			AND replica_instance.log_replica_updates then replica_instance.major_version else NULL end
		) AS count_distinct_logging_major_versions,
		primary_instance.is_disk_stalled != 0 AS is_disk_stalled
	FROM
		vitess_tablet
		JOIN vitess_keyspace ON (
			vitess_tablet.keyspace = vitess_keyspace.keyspace
		)
		JOIN vitess_shard ON (
			vitess_tablet.keyspace = vitess_shard.keyspace
			AND vitess_tablet.shard = vitess_shard.shard
		)
		LEFT JOIN database_instance primary_instance ON (
			vitess_tablet.alias = primary_instance.alias
			AND vitess_tablet.hostname = primary_instance.hostname
			AND vitess_tablet.port = primary_instance.port
		)
		LEFT JOIN vitess_tablet primary_tablet ON (
			primary_tablet.hostname = primary_instance.source_host
			AND primary_tablet.port = primary_instance.source_port
		)
		LEFT JOIN database_instance replica_instance ON (
			primary_instance.hostname = replica_instance.source_host
			AND primary_instance.port = replica_instance.source_port
		)
		LEFT JOIN database_instance_stale_binlog_coordinates ON (
			vitess_tablet.alias = database_instance_stale_binlog_coordinates.alias
		)
	WHERE
		? IN ('', vitess_keyspace.keyspace)
		AND ? IN ('', vitess_tablet.shard)
	GROUP BY
		vitess_tablet.alias
	ORDER BY
		vitess_tablet.tablet_type ASC,
		vitess_tablet.primary_timestamp DESC
	`

	clusters := make(map[string]*clusterAnalysis)
	err := db.Db.QueryVTOrc(query, args, func(m sqlutils.RowMap) error {
		a := &ReplicationAnalysis{
			Analysis: NoProblem,
		}

		tablet := &topodatapb.Tablet{}
		opts := prototext.UnmarshalOptions{DiscardUnknown: true}
		if err := opts.Unmarshal([]byte(m.GetString("tablet_info")), tablet); err != nil {
			log.Errorf("could not read tablet %v: %v", m.GetString("tablet_info"), err)
			return nil
		}

		// We don't want to run any fixes on any non-replica type tablet.
		if tablet.Type != topodatapb.TabletType_PRIMARY && !topo.IsReplicaType(tablet.Type) {
			return nil
		}

		primaryTablet := &topodatapb.Tablet{}
		if str := m.GetString("primary_tablet_info"); str != "" {
			if err := opts.Unmarshal([]byte(str), primaryTablet); err != nil {
				log.Errorf("could not read tablet %v: %v", str, err)
				return nil
			}
		}

		a.TabletType = tablet.Type
		a.CurrentTabletType = topodatapb.TabletType(m.GetInt("current_tablet_type"))
		a.AnalyzedKeyspace = m.GetString("keyspace")
		a.AnalyzedShard = m.GetString("shard")
		a.PrimaryTimeStamp = m.GetTime("primary_timestamp")

		if keyspaceType := topodatapb.KeyspaceType(m.GetInt32("keyspace_type")); keyspaceType == topodatapb.KeyspaceType_SNAPSHOT {
			log.Errorf("keyspace %v is a snapshot keyspace. Skipping.", a.AnalyzedKeyspace)
			return nil
		}

		a.ShardPrimaryTermTimestamp = m.GetTime("shard_primary_term_timestamp")
		a.IsPrimary = m.GetBool("is_primary")
		a.AnalyzedInstanceAlias = topoproto.TabletAliasString(tablet.Alias)
		a.AnalyzedInstancePrimaryAlias = topoproto.TabletAliasString(primaryTablet.Alias)
		a.AnalyzedInstanceBinlogCoordinates = BinlogCoordinates{
			LogFile: m.GetString("binary_log_file"),
			LogPos:  m.GetUint64("binary_log_pos"),
			Type:    BinaryLog,
		}
		isStaleBinlogCoordinates := m.GetBool("is_stale_binlog_coordinates")
		a.GTIDMode = m.GetString("gtid_mode")
		a.LastCheckValid = m.GetBool("is_last_check_valid")
		a.LastCheckPartialSuccess = m.GetBool("last_check_partial_success")
		a.CountReplicas = m.GetUint("count_replicas")
		a.CountValidReplicas = m.GetUint("count_valid_replicas")
		a.CountValidReplicatingReplicas = m.GetUint("count_valid_replicating_replicas")
		a.ReplicationStopped = m.GetBool("replication_stopped")
		a.ErrantGTID = m.GetString("gtid_errant")

		countValidOracleGTIDReplicas := m.GetUint("count_valid_oracle_gtid_replicas")
		a.OracleGTIDImmediateTopology = countValidOracleGTIDReplicas == a.CountValidReplicas && a.CountValidReplicas > 0
		countValidBinlogServerReplicas := m.GetUint("count_valid_binlog_server_replicas")
		a.BinlogServerImmediateTopology = countValidBinlogServerReplicas == a.CountValidReplicas && a.CountValidReplicas > 0
		a.SemiSyncPrimaryEnabled = m.GetBool("semi_sync_primary_enabled")
		a.SemiSyncPrimaryStatus = m.GetBool("semi_sync_primary_status")
		a.SemiSyncBlocked = m.GetBool("semi_sync_blocked")
		a.SemiSyncReplicaEnabled = m.GetBool("semi_sync_replica_enabled")
		a.CountSemiSyncReplicasEnabled = m.GetUint("count_semi_sync_replicas")
		// countValidSemiSyncReplicasEnabled := m.GetUint("count_valid_semi_sync_replicas")
		a.SemiSyncPrimaryWaitForReplicaCount = m.GetUint("semi_sync_primary_wait_for_replica_count")
		a.SemiSyncPrimaryClients = m.GetUint("semi_sync_primary_clients")

		a.MinReplicaGTIDMode = m.GetString("min_replica_gtid_mode")
		a.MaxReplicaGTIDMode = m.GetString("max_replica_gtid_mode")
		a.MaxReplicaGTIDErrant = m.GetString("max_replica_gtid_errant")

		a.CountLoggingReplicas = m.GetUint("count_logging_replicas")
		a.CountStatementBasedLoggingReplicas = m.GetUint("count_statement_based_logging_replicas")
		a.CountMixedBasedLoggingReplicas = m.GetUint("count_mixed_based_logging_replicas")
		a.CountRowBasedLoggingReplicas = m.GetUint("count_row_based_logging_replicas")
		a.CountDistinctMajorVersionsLoggingReplicas = m.GetUint("count_distinct_logging_major_versions")

		a.CountDelayedReplicas = m.GetUint("count_delayed_replicas")
		a.CountLaggingReplicas = m.GetUint("count_lagging_replicas")
		a.ReplicaNetTimeout = m.GetInt32("replica_net_timeout")
		a.HeartbeatInterval = m.GetFloat64("heartbeat_interval")

		a.IsReadOnly = m.GetUint("read_only") == 1
		a.IsDiskStalled = m.GetBool("is_disk_stalled")

		if !a.LastCheckValid {
			analysisMessage := fmt.Sprintf("analysis: Alias: %+v, Keyspace: %+v, Shard: %+v, IsPrimary: %+v, LastCheckValid: %+v, LastCheckPartialSuccess: %+v, CountReplicas: %+v, CountValidReplicas: %+v, CountValidReplicatingReplicas: %+v, CountLaggingReplicas: %+v, CountDelayedReplicas: %+v",
				a.AnalyzedInstanceAlias, a.AnalyzedKeyspace, a.AnalyzedShard, a.IsPrimary, a.LastCheckValid, a.LastCheckPartialSuccess, a.CountReplicas, a.CountValidReplicas, a.CountValidReplicatingReplicas, a.CountLaggingReplicas, a.CountDelayedReplicas,
			)
			if util.ClearToLog("analysis_dao", analysisMessage) {
				log.Infof(analysisMessage)
			}
		}
		keyspaceShard := getKeyspaceShardName(a.AnalyzedKeyspace, a.AnalyzedShard)
		if clusters[keyspaceShard] == nil {
			clusters[keyspaceShard] = &clusterAnalysis{}
			if a.TabletType == topodatapb.TabletType_PRIMARY {
				a.IsClusterPrimary = true
				clusters[keyspaceShard].primaryAlias = a.AnalyzedInstanceAlias
			}
			durabilityPolicy := m.GetString("durability_policy")
			if durabilityPolicy == "" {
				log.Errorf("ignoring keyspace %v because no durability_policy is set. Please set it using SetKeyspaceDurabilityPolicy", a.AnalyzedKeyspace)
				return nil
			}
			durability, err := policy.GetDurabilityPolicy(durabilityPolicy)
			if err != nil {
				log.Errorf("can't get the durability policy %v - %v. Skipping keyspace - %v.", durabilityPolicy, err, a.AnalyzedKeyspace)
				return nil
			}
			clusters[keyspaceShard].durability = durability
		}
		// ca has clusterwide info
		ca := clusters[keyspaceShard]
		// Increment the total number of tablets.
		ca.totalTablets += 1
		if ca.hasShardWideAction {
			// We can only take one shard level action at a time.
			return nil
		}
		if ca.durability == nil {
			// We failed to load the durability policy, so we shouldn't run any analysis
			return nil
		}
		isInvalid := m.GetBool("is_invalid")
		if a.IsClusterPrimary && isInvalid {
			a.Analysis = InvalidPrimary
			a.Description = "VTOrc hasn't been able to reach the primary even once since restart/shutdown"
		} else if isInvalid {
			a.Analysis = InvalidReplica
			a.Description = "VTOrc hasn't been able to reach the replica even once since restart/shutdown"
		} else if a.IsClusterPrimary && !a.LastCheckValid && a.IsDiskStalled {
			a.Analysis = PrimaryDiskStalled
			a.Description = "Primary has a stalled disk"
			ca.hasShardWideAction = true
		} else if a.IsClusterPrimary && !a.LastCheckValid && a.CountReplicas == 0 {
			a.Analysis = DeadPrimaryWithoutReplicas
			a.Description = "Primary cannot be reached by vtorc and has no replica"
			ca.hasShardWideAction = true
			//
		} else if a.IsClusterPrimary && !a.LastCheckValid && a.CountValidReplicas == a.CountReplicas && a.CountValidReplicatingReplicas == 0 {
			a.Analysis = DeadPrimary
			a.Description = "Primary cannot be reached by vtorc and none of its replicas is replicating"
			ca.hasShardWideAction = true
			//
		} else if a.IsClusterPrimary && !a.LastCheckValid && a.CountReplicas > 0 && a.CountValidReplicas == 0 && a.CountValidReplicatingReplicas == 0 {
			a.Analysis = DeadPrimaryAndReplicas
			a.Description = "Primary cannot be reached by vtorc and none of its replicas is replicating"
			ca.hasShardWideAction = true
			//
		} else if a.IsClusterPrimary && !a.LastCheckValid && a.CountValidReplicas < a.CountReplicas && a.CountValidReplicas > 0 && a.CountValidReplicatingReplicas == 0 {
			a.Analysis = DeadPrimaryAndSomeReplicas
			a.Description = "Primary cannot be reached by vtorc; some of its replicas are unreachable and none of its reachable replicas is replicating"
			ca.hasShardWideAction = true
			//
		} else if a.IsClusterPrimary && !a.IsPrimary {
			a.Analysis = PrimaryHasPrimary
			a.Description = "Primary is replicating from somewhere else"
			ca.hasShardWideAction = true
			//
		} else if a.IsClusterPrimary && a.IsReadOnly {
			a.Analysis = PrimaryIsReadOnly
			a.Description = "Primary is read-only"
			//
		} else if a.IsClusterPrimary && policy.SemiSyncAckers(ca.durability, tablet) != 0 && !a.SemiSyncPrimaryEnabled {
			a.Analysis = PrimarySemiSyncMustBeSet
			a.Description = "Primary semi-sync must be set"
			//
		} else if a.IsClusterPrimary && policy.SemiSyncAckers(ca.durability, tablet) == 0 && a.SemiSyncPrimaryEnabled {
			a.Analysis = PrimarySemiSyncMustNotBeSet
			a.Description = "Primary semi-sync must not be set"
			//
		} else if a.IsClusterPrimary && a.CurrentTabletType != topodatapb.TabletType_UNKNOWN && a.CurrentTabletType != topodatapb.TabletType_PRIMARY {
			a.Analysis = PrimaryCurrentTypeMismatch
			a.Description = "Primary tablet's current type is not PRIMARY"
		} else if topo.IsReplicaType(a.TabletType) && a.ErrantGTID != "" {
			a.Analysis = ErrantGTIDDetected
			a.Description = "Tablet has errant GTIDs"
		} else if topo.IsReplicaType(a.TabletType) && ca.primaryAlias == "" && a.ShardPrimaryTermTimestamp.IsZero() {
			// ClusterHasNoPrimary should only be detected when the shard record doesn't have any primary term start time specified either.
			a.Analysis = ClusterHasNoPrimary
			a.Description = "Cluster has no primary"
			ca.hasShardWideAction = true
		} else if topo.IsReplicaType(a.TabletType) && ca.primaryAlias == "" && !a.ShardPrimaryTermTimestamp.IsZero() {
			// If there are no primary tablets, but the shard primary start time isn't empty, then we know
			// the primary tablet was deleted.
			a.Analysis = PrimaryTabletDeleted
			a.Description = "Primary tablet has been deleted"
			ca.hasShardWideAction = true
		} else if a.IsPrimary && a.SemiSyncBlocked && a.CountSemiSyncReplicasEnabled >= a.SemiSyncPrimaryWaitForReplicaCount {
			// The primary is reporting that semi-sync monitor is blocked on writes.
			// There are enough replicas configured to send semi-sync ACKs such that the primary shouldn't be blocked.
			// There is some network diruption in progress. We should run an ERS.
			a.Analysis = PrimarySemiSyncBlocked
			a.Description = "Writes seem to be blocked on semi-sync acks on the primary, even though sufficient replicas are configured to send ACKs"
			ca.hasShardWideAction = true
		} else if topo.IsReplicaType(a.TabletType) && !a.IsReadOnly {
			a.Analysis = ReplicaIsWritable
			a.Description = "Replica is writable"
			//
		} else if topo.IsReplicaType(a.TabletType) && a.IsPrimary {
			a.Analysis = NotConnectedToPrimary
			a.Description = "Not connected to the primary"
			//
		} else if topo.IsReplicaType(a.TabletType) && !a.IsPrimary && math.Round(a.HeartbeatInterval*2) != float64(a.ReplicaNetTimeout) {
			a.Analysis = ReplicaMisconfigured
			a.Description = "Replica has been misconfigured"
			//
		} else if topo.IsReplicaType(a.TabletType) && !a.IsPrimary && ca.primaryAlias != "" && a.AnalyzedInstancePrimaryAlias != ca.primaryAlias {
			a.Analysis = ConnectedToWrongPrimary
			a.Description = "Connected to wrong primary"
			//
		} else if topo.IsReplicaType(a.TabletType) && !a.IsPrimary && a.ReplicationStopped {
			a.Analysis = ReplicationStopped
			a.Description = "Replication is stopped"
			//
		} else if topo.IsReplicaType(a.TabletType) && !a.IsPrimary && policy.IsReplicaSemiSync(ca.durability, primaryTablet, tablet) && !a.SemiSyncReplicaEnabled {
			a.Analysis = ReplicaSemiSyncMustBeSet
			a.Description = "Replica semi-sync must be set"
			//
		} else if topo.IsReplicaType(a.TabletType) && !a.IsPrimary && !policy.IsReplicaSemiSync(ca.durability, primaryTablet, tablet) && a.SemiSyncReplicaEnabled {
			a.Analysis = ReplicaSemiSyncMustNotBeSet
			a.Description = "Replica semi-sync must not be set"
			//
			// TODO(sougou): Events below here are either ignored or not possible.
		} else if a.IsPrimary && !a.LastCheckValid && a.CountLaggingReplicas == a.CountReplicas && a.CountDelayedReplicas < a.CountReplicas && a.CountValidReplicatingReplicas > 0 {
			a.Analysis = UnreachablePrimaryWithLaggingReplicas
			a.Description = "Primary cannot be reached by vtorc and all of its replicas are lagging"
			//
		} else if a.IsPrimary && !a.LastCheckValid && !a.LastCheckPartialSuccess && a.CountValidReplicas > 0 && a.CountValidReplicatingReplicas > 0 {
			// partial success is here to reduce noise
			a.Analysis = UnreachablePrimary
			a.Description = "Primary cannot be reached by vtorc but it has replicating replicas; possibly a network/host issue"
			//
		} else if a.IsPrimary && a.SemiSyncPrimaryEnabled && a.SemiSyncPrimaryStatus && a.SemiSyncPrimaryWaitForReplicaCount > 0 && a.SemiSyncPrimaryClients < a.SemiSyncPrimaryWaitForReplicaCount {
			if isStaleBinlogCoordinates {
				a.Analysis = LockedSemiSyncPrimary
				a.Description = "Semi sync primary is locked since it doesn't get enough replica acknowledgements"
			} else {
				a.Analysis = LockedSemiSyncPrimaryHypothesis
				a.Description = "Semi sync primary seems to be locked, more samplings needed to validate"
			}
			//
		} else if a.IsPrimary && a.LastCheckValid && a.CountReplicas == 1 && a.CountValidReplicas == a.CountReplicas && a.CountValidReplicatingReplicas == 0 {
			a.Analysis = PrimarySingleReplicaNotReplicating
			a.Description = "Primary is reachable but its single replica is not replicating"
		} else if a.IsPrimary && a.LastCheckValid && a.CountReplicas == 1 && a.CountValidReplicas == 0 {
			a.Analysis = PrimarySingleReplicaDead
			a.Description = "Primary is reachable but its single replica is dead"
			//
		} else if a.IsPrimary && a.LastCheckValid && a.CountReplicas > 1 && a.CountValidReplicas == a.CountReplicas && a.CountValidReplicatingReplicas == 0 {
			a.Analysis = AllPrimaryReplicasNotReplicating
			a.Description = "Primary is reachable but none of its replicas is replicating"
			//
		} else if a.IsPrimary && a.LastCheckValid && a.CountReplicas > 1 && a.CountValidReplicas < a.CountReplicas && a.CountValidReplicas > 0 && a.CountValidReplicatingReplicas == 0 {
			a.Analysis = AllPrimaryReplicasNotReplicatingOrDead
			a.Description = "Primary is reachable but none of its replicas is replicating"
			//
		}
		//		 else if a.IsPrimary && a.CountReplicas == 0 {
		//			a.Analysis = PrimaryWithoutReplicas
		//			a.Description = "Primary has no replicas"
		//		}

		{
			// Moving on to structure analysis
			// We also do structural checks. See if there's potential danger in promotions
			if a.IsPrimary && a.CountLoggingReplicas == 0 && a.CountReplicas > 1 {
				a.StructureAnalysis = append(a.StructureAnalysis, NoLoggingReplicasStructureWarning)
			}
			if a.IsPrimary && a.CountReplicas > 1 &&
				!a.OracleGTIDImmediateTopology &&
				!a.BinlogServerImmediateTopology {
				a.StructureAnalysis = append(a.StructureAnalysis, NoFailoverSupportStructureWarning)
			}
			if a.IsPrimary && a.CountStatementBasedLoggingReplicas > 0 && a.CountMixedBasedLoggingReplicas > 0 {
				a.StructureAnalysis = append(a.StructureAnalysis, StatementAndMixedLoggingReplicasStructureWarning)
			}
			if a.IsPrimary && a.CountStatementBasedLoggingReplicas > 0 && a.CountRowBasedLoggingReplicas > 0 {
				a.StructureAnalysis = append(a.StructureAnalysis, StatementAndRowLoggingReplicasStructureWarning)
			}
			if a.IsPrimary && a.CountMixedBasedLoggingReplicas > 0 && a.CountRowBasedLoggingReplicas > 0 {
				a.StructureAnalysis = append(a.StructureAnalysis, MixedAndRowLoggingReplicasStructureWarning)
			}
			if a.IsPrimary && a.CountDistinctMajorVersionsLoggingReplicas > 1 {
				a.StructureAnalysis = append(a.StructureAnalysis, MultipleMajorVersionsLoggingReplicasStructureWarning)
			}

			if a.CountReplicas > 0 && (a.GTIDMode != a.MinReplicaGTIDMode || a.GTIDMode != a.MaxReplicaGTIDMode) {
				a.StructureAnalysis = append(a.StructureAnalysis, DifferentGTIDModesStructureWarning)
			}
			if a.MaxReplicaGTIDErrant != "" {
				a.StructureAnalysis = append(a.StructureAnalysis, ErrantGTIDStructureWarning)
			}

			if a.IsPrimary && a.IsReadOnly {
				a.StructureAnalysis = append(a.StructureAnalysis, NoWriteablePrimaryStructureWarning)
			}

			if a.IsPrimary && a.SemiSyncPrimaryEnabled && !a.SemiSyncPrimaryStatus && a.SemiSyncPrimaryWaitForReplicaCount > 0 && a.SemiSyncPrimaryClients < a.SemiSyncPrimaryWaitForReplicaCount {
				a.StructureAnalysis = append(a.StructureAnalysis, NotEnoughValidSemiSyncReplicasStructureWarning)
			}
		}
		appendAnalysis(a)

		if a.CountReplicas > 0 && hints.AuditAnalysis {
			// Interesting enough for analysis
			go func() {
				_ = auditInstanceAnalysisInChangelog(a.AnalyzedInstanceAlias, a.Analysis)
			}()
		}
		return nil
	})

	result = postProcessAnalyses(result, clusters)

	if err != nil {
		log.Error(err)
	}
	// TODO: result, err = getConcensusReplicationAnalysis(result)
	return result, err
}

// postProcessAnalyses is used to update different analyses based on the information gleaned from looking at all the analyses together instead of individual data.
func postProcessAnalyses(result []*ReplicationAnalysis, clusters map[string]*clusterAnalysis) []*ReplicationAnalysis {
	for {
		// Store whether we have changed the result of replication analysis or not.
		resultChanged := false

		// Go over all the analyses.
		for _, analysis := range result {
			// If one of them is an InvalidPrimary, then we see if all the other tablets in this keyspace shard are
			// unable to replicate or not.
			if analysis.Analysis == InvalidPrimary {
				keyspaceName := analysis.AnalyzedKeyspace
				shardName := analysis.AnalyzedShard
				keyspaceShard := getKeyspaceShardName(keyspaceName, shardName)
				totalReplicas := clusters[keyspaceShard].totalTablets - 1
				var notReplicatingReplicas []int
				for idx, replicaAnalysis := range result {
					if replicaAnalysis.AnalyzedKeyspace == keyspaceName &&
						replicaAnalysis.AnalyzedShard == shardName && topo.IsReplicaType(replicaAnalysis.TabletType) {
						// If the replica's last check is invalid or its replication is stopped, then we consider as not replicating.
						if !replicaAnalysis.LastCheckValid || replicaAnalysis.ReplicationStopped {
							notReplicatingReplicas = append(notReplicatingReplicas, idx)
						}
					}
				}
				// If none of the other tablets are able to replicate, then we conclude that this primary is not just Invalid, but also Dead.
				// In this case, we update the analysis for the primary tablet and remove all the analyses of the replicas.
				if totalReplicas > 0 && len(notReplicatingReplicas) == totalReplicas {
					resultChanged = true
					analysis.Analysis = DeadPrimary
					for i := len(notReplicatingReplicas) - 1; i >= 0; i-- {
						idxToRemove := notReplicatingReplicas[i]
						result = append(result[0:idxToRemove], result[idxToRemove+1:]...)
					}
					break
				}
			}
		}
		if !resultChanged {
			break
		}
	}
	return result
}

// auditInstanceAnalysisInChangelog will write down an instance's analysis in the database_instance_analysis_changelog table.
// To not repeat recurring analysis code, the database_instance_last_analysis table is used, so that only changes to
// analysis codes are written.
func auditInstanceAnalysisInChangelog(tabletAlias string, analysisCode AnalysisCode) error {
	if lastWrittenAnalysis, found := recentInstantAnalysis.Get(tabletAlias); found {
		if lastWrittenAnalysis == analysisCode {
			// Surely nothing new.
			// And let's expand the timeout
			recentInstantAnalysis.Set(tabletAlias, analysisCode, cache.DefaultExpiration)
			return nil
		}
	}

	// Find if the lastAnalysisHasChanged or not while updating the row if it has.
	lastAnalysisChanged := false
	{
		sqlResult, err := db.ExecVTOrc(`UPDATE database_instance_last_analysis
			SET
				analysis = ?,
				analysis_timestamp = DATETIME('now')
			WHERE
				alias = ?
				AND analysis != ?
			`,
			string(analysisCode), tabletAlias, string(analysisCode),
		)
		if err != nil {
			log.Error(err)
			return err
		}
		rows, err := sqlResult.RowsAffected()
		if err != nil {
			log.Error(err)
			return err
		}
		lastAnalysisChanged = rows > 0
	}

	// If the last analysis has not changed, then there is a chance that this is the first insertion.
	// We need to find that out too when we insert into the database.
	firstInsertion := false
	if !lastAnalysisChanged {
		// The insert only returns more than 1 row changed if this is the first insertion.
		sqlResult, err := db.ExecVTOrc(`INSERT OR IGNORE
			INTO database_instance_last_analysis (
				alias,
				analysis_timestamp,
				analysis
			) VALUES (
				?,
				DATETIME('now'),
				?
			)`,
			tabletAlias, string(analysisCode),
		)
		if err != nil {
			log.Error(err)
			return err
		}
		rows, err := sqlResult.RowsAffected()
		if err != nil {
			log.Error(err)
			return err
		}
		firstInsertion = rows > 0
	}
	recentInstantAnalysis.Set(tabletAlias, analysisCode, cache.DefaultExpiration)
	// If the analysis has changed or if it is the first insertion, we need to make sure we write this change to the database.
	if !lastAnalysisChanged && !firstInsertion {
		return nil
	}

	_, err := db.ExecVTOrc(`INSERT
		INTO database_instance_analysis_changelog (
			alias,
			analysis_timestamp,
			analysis
		) VALUES (
			?,
			DATETIME('now'),
			?
		)`,
		tabletAlias, string(analysisCode),
	)
	if err == nil {
		analysisChangeWriteCounter.Add(1)
	} else {
		log.Error(err)
	}
	return err
}

// ExpireInstanceAnalysisChangelog removes old-enough analysis entries from the changelog
func ExpireInstanceAnalysisChangelog() error {
	_, err := db.ExecVTOrc(`DELETE
		FROM database_instance_analysis_changelog
		WHERE
			analysis_timestamp < DATETIME('now', PRINTF('-%d HOUR', ?))
		`,
		config.UnseenInstanceForgetHours,
	)
	if err != nil {
		log.Error(err)
	}
	return err
}
