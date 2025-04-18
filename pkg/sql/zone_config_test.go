// Copyright 2015 The Cockroach Authors.
//
// Use of this software is governed by the CockroachDB Software License
// included in the /LICENSE file.

package sql_test

import (
	"context"
	"testing"

	"github.com/cockroachdb/cockroach/pkg/base"
	"github.com/cockroachdb/cockroach/pkg/config"
	"github.com/cockroachdb/cockroach/pkg/config/zonepb"
	"github.com/cockroachdb/cockroach/pkg/keys"
	"github.com/cockroachdb/cockroach/pkg/kv"
	"github.com/cockroachdb/cockroach/pkg/roachpb"
	"github.com/cockroachdb/cockroach/pkg/server"
	"github.com/cockroachdb/cockroach/pkg/sql"
	"github.com/cockroachdb/cockroach/pkg/sql/catalog/bootstrap"
	"github.com/cockroachdb/cockroach/pkg/sql/catalog/catalogkeys"
	"github.com/cockroachdb/cockroach/pkg/sql/catalog/catpb"
	"github.com/cockroachdb/cockroach/pkg/sql/catalog/descpb"
	"github.com/cockroachdb/cockroach/pkg/sql/catalog/descs"
	"github.com/cockroachdb/cockroach/pkg/sql/catalog/systemschema"
	"github.com/cockroachdb/cockroach/pkg/sql/isql"
	"github.com/cockroachdb/cockroach/pkg/sql/sqltestutils"
	"github.com/cockroachdb/cockroach/pkg/testutils"
	"github.com/cockroachdb/cockroach/pkg/testutils/serverutils"
	"github.com/cockroachdb/cockroach/pkg/testutils/sqlutils"
	"github.com/cockroachdb/cockroach/pkg/util/leaktest"
	"github.com/cockroachdb/cockroach/pkg/util/log"
	"github.com/cockroachdb/cockroach/pkg/util/protoutil"
	"github.com/cockroachdb/errors"
	"github.com/gogo/protobuf/proto"
	"github.com/stretchr/testify/require"
)

var configID = descpb.ID(1)

// forceNewConfig forces a system config update by writing a bogus descriptor with an
// incremented value inside. It then repeatedly fetches the gossip config until the
// just-written descriptor is found.
func forceNewConfig(t testing.TB, s serverutils.TestServerInterface) *config.SystemConfig {
	var configDescKey = catalogkeys.MakeDescMetadataKey(
		s.Codec(), descpb.ID(bootstrap.TestingUserDescID(0)))

	configID++
	configDesc := &descpb.Descriptor{
		Union: &descpb.Descriptor_Database{
			Database: &descpb.DatabaseDescriptor{
				Name:       "sentinel",
				ID:         configID,
				Privileges: &catpb.PrivilegeDescriptor{},
			},
		},
	}

	// This needs to be done in a transaction with the system trigger set.
	if err := s.DB().Txn(context.Background(), func(ctx context.Context, txn *kv.Txn) error {
		return txn.Put(ctx, configDescKey, configDesc)
	}); err != nil {
		t.Fatal(err)
	}

	var cfg *config.SystemConfig
	testutils.SucceedsSoon(t, func() error {
		if cfg = s.ApplicationLayer().SystemConfigProvider().GetSystemConfig(); cfg != nil {
			if val := cfg.GetValue(configDescKey); val != nil {
				var foundDesc descpb.Descriptor
				if err := val.GetProto(&foundDesc); err != nil {
					t.Fatal(err)
				}
				_, db, _, _, _ := descpb.GetDescriptors(&foundDesc)
				if db.ID != configID {
					return errors.Errorf("expected database id %d; got %d", configID, db.ID)
				}
				return nil
			}
		}
		return errors.Errorf("got nil system config")
	})
	return cfg
}

func TestGetZoneConfig(t *testing.T) {
	defer leaktest.AfterTest(t)()
	defer log.Scope(t).Close(t)
	params, _ := createTestServerParamsAllowTenants()
	defaultZoneConfig := zonepb.DefaultSystemZoneConfig()
	defaultZoneConfig.NumReplicas = proto.Int32(1)
	defaultZoneConfig.RangeMinBytes = proto.Int64(1 << 20)
	defaultZoneConfig.RangeMaxBytes = proto.Int64(100 << 20)
	defaultZoneConfig.GC = &zonepb.GCPolicy{TTLSeconds: 60}
	require.NoError(t, defaultZoneConfig.Validate())
	params.Knobs.Server = &server.TestingKnobs{
		DefaultZoneConfigOverride:       &defaultZoneConfig,
		DefaultSystemZoneConfigOverride: &defaultZoneConfig,
	}

	s, sqlDB, _ := serverutils.StartServer(t, params)
	defer s.Stopper().Stop(context.Background())
	// Set the closed_timestamp interval to be short to shorten the test duration.
	systemDB := sqlutils.MakeSQLRunner(s.SystemLayer().SQLConn(t))
	systemDB.Exec(t, `SET CLUSTER SETTING kv.closed_timestamp.target_duration = '20ms'`)
	systemDB.Exec(t, `SET CLUSTER SETTING kv.closed_timestamp.side_transport_interval = '20ms'`)
	systemDB.Exec(t, `SET CLUSTER SETTING kv.rangefeed.closed_timestamp_refresh_interval = '20ms'`)

	codec := s.Codec()

	type testCase struct {
		objectID uint32

		// keySuffix and partitionName must specify the same subzone.
		keySuffix     []byte
		partitionName string

		zoneCfg zonepb.ZoneConfig
	}
	verifyZoneConfigs := func(testCases []testCase) {
		cfg := forceNewConfig(t, s)

		for tcNum, tc := range testCases {
			// Verify SystemConfig.GetZoneConfigForKey. GetZoneConfigForKey is
			// exclusive to the system tenant.
			if !s.StartedDefaultTestTenant() {
				key := append(roachpb.RKey(codec.TablePrefix(tc.objectID)), tc.keySuffix...)
				_, zoneCfg, err := config.TestingGetSystemTenantZoneConfigForKey(cfg, key) // Complete ZoneConfig
				if err != nil {
					t.Fatalf("#%d: err=%s", tcNum, err)
				}

				if !tc.zoneCfg.Equal(zoneCfg) {
					t.Errorf("#%d: bad zone config.\nexpected: %+v\ngot: %+v", tcNum, tc.zoneCfg, zoneCfg)
				}
			}

			// Verify sql.GetZoneConfigInTxn.
			dummyIndex := systemschema.CommentsTable.GetPrimaryIndex()
			if err := sqltestutils.TestingDescsTxn(context.Background(), s, func(ctx context.Context, txn isql.Txn, col *descs.Collection) error {
				_, zoneCfg, subzone, err := sql.GetZoneConfigInTxn(
					ctx, txn.KV(), col, descpb.ID(tc.objectID), dummyIndex, tc.partitionName, false,
				)
				if err != nil {
					return err
				} else if subzone != nil {
					zoneCfg = &subzone.Config
				}
				if !tc.zoneCfg.Equal(zoneCfg) {
					t.Errorf("#%d: bad zone config.\nexpected: %+v\ngot: %+v", tcNum, tc.zoneCfg, zoneCfg)
				}
				return nil
			}); err != nil {
				t.Fatalf("#%d: err=%s", tcNum, err)
			}
		}
	}

	{
		buf, err := protoutil.Marshal(&defaultZoneConfig)
		if err != nil {
			t.Fatal(err)
		}
		objID := keys.RootNamespaceID
		if _, err = sqlDB.Exec(`UPDATE system.zones SET config = $2 WHERE id = $1`, objID, buf); err != nil {
			t.Fatalf("problem writing zone %+v: %s", defaultZoneConfig, err)
		}
	}

	// Naming scheme for database and tables:
	// db1 has tables tb11 and tb12
	// db2 has tables tb21 and tb22

	if _, err := sqlDB.Exec(`CREATE DATABASE db1`); err != nil {
		t.Fatal(err)
	}

	if _, err := sqlDB.Exec(`CREATE DATABASE db2`); err != nil {
		t.Fatal(err)
	}

	if _, err := sqlDB.Exec(`CREATE TABLE db1.tb1 (k INT PRIMARY KEY, v INT)`); err != nil {
		t.Fatal(err)
	}

	if _, err := sqlDB.Exec(`CREATE TABLE db1.tb2 (k INT PRIMARY KEY, v INT)`); err != nil {
		t.Fatal(err)
	}

	if _, err := sqlDB.Exec(`CREATE TABLE db2.tb1 (k INT PRIMARY KEY, v INT)`); err != nil {
		t.Fatal(err)
	}

	if _, err := sqlDB.Exec(`CREATE TABLE db2.tb2 (k INT PRIMARY KEY, v INT)`); err != nil {
		t.Fatal(err)
	}

	if _, err := sqlDB.Exec(`TRUNCATE TABLE db2.tb2`); err != nil {
		t.Fatal(err)
	}

	db1 := sqlutils.QueryDatabaseID(t, sqlDB, "db1")
	db2 := sqlutils.QueryDatabaseID(t, sqlDB, "db2")
	tb11 := sqlutils.QueryTableID(t, sqlDB, "db1", "public", "tb1")
	tb12 := sqlutils.QueryTableID(t, sqlDB, "db1", "public", "tb2")
	tb21 := sqlutils.QueryTableID(t, sqlDB, "db2", "public", "tb1")
	tb22 := sqlutils.QueryTableID(t, sqlDB, "db2", "public", "tb2")

	t.Logf("Verifying with no custom zone configs")
	verifyZoneConfigs([]testCase{
		{keys.RootNamespaceID, nil, "", defaultZoneConfig},
		{keys.SystemDatabaseID, nil, "", defaultZoneConfig},
		{db1, nil, "", defaultZoneConfig},
		{db2, nil, "", defaultZoneConfig},
		{tb11, nil, "", defaultZoneConfig},
		{tb11, []byte{42}, "p0", defaultZoneConfig},
		{tb12, nil, "", defaultZoneConfig},
		{tb12, []byte{42}, "p0", defaultZoneConfig},
		{tb21, nil, "", defaultZoneConfig},
		{tb22, nil, "", defaultZoneConfig},
	})

	// Now set some zone configs. We don't have a nice way of using table
	// names for this, so we do raw puts.
	// Here is the list of dbs/tables/partitions and whether they have a custom
	// zone config:
	// db1: true
	//   tb1: true
	//   tb2: false
	// db2: false
	//   tb1: true
	//     p1: true [1, 2), [6, 7)
	//     p2: true [3, 5)
	//   tb2: false
	//     p1: true  [1, 255)

	db1Cfg := defaultZoneConfig
	db1Cfg.NumReplicas = proto.Int32(1)
	db1Cfg.Constraints = []zonepb.ConstraintsConjunction{{Constraints: []zonepb.Constraint{{Type: zonepb.Constraint_REQUIRED, Value: "db1"}}}}

	tb11Cfg := defaultZoneConfig
	tb11Cfg.NumReplicas = proto.Int32(1)
	tb11Cfg.Constraints = []zonepb.ConstraintsConjunction{{Constraints: []zonepb.Constraint{{Type: zonepb.Constraint_REQUIRED, Value: "db1.tb1"}}}}

	p211Cfg := defaultZoneConfig
	p211Cfg.NumReplicas = proto.Int32(1)
	p211Cfg.Constraints = []zonepb.ConstraintsConjunction{{Constraints: []zonepb.Constraint{{Type: zonepb.Constraint_REQUIRED, Value: "db2.tb1.p1"}}}}

	p212Cfg := defaultZoneConfig
	p212Cfg.NumReplicas = proto.Int32(1)
	p212Cfg.Constraints = []zonepb.ConstraintsConjunction{{Constraints: []zonepb.Constraint{{Type: zonepb.Constraint_REQUIRED, Value: "db2.tb1.p2"}}}}

	tb21Cfg := defaultZoneConfig
	tb21Cfg.NumReplicas = proto.Int32(1)
	tb21Cfg.Constraints = []zonepb.ConstraintsConjunction{{Constraints: []zonepb.Constraint{{Type: zonepb.Constraint_REQUIRED, Value: "db2.tb1"}}}}
	tb21Cfg.Subzones = []zonepb.Subzone{
		{IndexID: 1, PartitionName: "p0", Config: p211Cfg},
		{IndexID: 1, PartitionName: "p1", Config: p212Cfg},
	}
	tb21Cfg.SubzoneSpans = []zonepb.SubzoneSpan{
		{SubzoneIndex: 0, Key: []byte{1}},
		{SubzoneIndex: 1, Key: []byte{3}, EndKey: []byte{5}},
		{SubzoneIndex: 0, Key: []byte{6}},
	}

	p221Cfg := defaultZoneConfig
	p221Cfg.NumReplicas = proto.Int32(1)
	p221Cfg.Constraints = []zonepb.ConstraintsConjunction{{Constraints: []zonepb.Constraint{{Type: zonepb.Constraint_REQUIRED, Value: "db2.tb2.p1"}}}}

	// Subzone Placeholder
	tb22Cfg := *zonepb.NewZoneConfig()
	tb22Cfg.NumReplicas = proto.Int32(0)
	tb22Cfg.Subzones = []zonepb.Subzone{{IndexID: 1, PartitionName: "p0", Config: p221Cfg}}
	tb22Cfg.SubzoneSpans = []zonepb.SubzoneSpan{
		{SubzoneIndex: 0, Key: []byte{1}, EndKey: []byte{255}},
	}

	for objID, objZone := range map[uint32]zonepb.ZoneConfig{
		db1:  db1Cfg,
		tb11: tb11Cfg,
		tb21: tb21Cfg,
		tb22: tb22Cfg,
	} {
		buf, err := protoutil.Marshal(&objZone)
		if err != nil {
			t.Fatal(err)
		}
		if _, err = sqlDB.Exec(`INSERT INTO system.zones VALUES ($1, $2)`, objID, buf); err != nil {
			t.Fatalf("problem writing zone %+v: %s", objZone, err)
		}
	}

	t.Logf("Verifying with custom zone configs")
	verifyZoneConfigs([]testCase{
		{keys.RootNamespaceID, nil, "", defaultZoneConfig},
		{keys.SystemDatabaseID, nil, "", defaultZoneConfig},
		{db1, nil, "", db1Cfg},
		{db2, nil, "", defaultZoneConfig},
		{tb11, nil, "", tb11Cfg},
		{tb11, []byte{42}, "p0", tb11Cfg},
		{tb12, nil, "", db1Cfg},
		{tb12, []byte{42}, "p0", db1Cfg},
		{tb21, nil, "", tb21Cfg},
		{tb21, []byte{}, "", tb21Cfg},
		{tb21, []byte{0}, "", tb21Cfg},
		{tb21, []byte{1}, "p0", p211Cfg},
		{tb21, []byte{1, 255}, "p0", p211Cfg},
		{tb21, []byte{2}, "", tb21Cfg},
		{tb21, []byte{3}, "p1", p212Cfg},
		{tb21, []byte{4}, "p1", p212Cfg},
		{tb21, []byte{5}, "", tb21Cfg},
		{tb21, []byte{6}, "p0", p211Cfg},
		{tb22, nil, "", defaultZoneConfig},
		{tb22, []byte{0}, "", defaultZoneConfig},
		{tb22, []byte{1}, "p0", p221Cfg},
		{tb22, []byte{255}, "", defaultZoneConfig},
	})
}

// TestCascadingZoneConfig tests whether the cascading nature of
// the zone configurations works well with the different inheritance
// hierarchies.
func TestCascadingZoneConfig(t *testing.T) {
	defer leaktest.AfterTest(t)()
	defer log.Scope(t).Close(t)
	params, _ := createTestServerParamsAllowTenants()

	defaultZoneConfig := zonepb.DefaultZoneConfig()
	defaultZoneConfig.NumReplicas = proto.Int32(1)
	defaultZoneConfig.RangeMinBytes = proto.Int64(1 << 20)
	defaultZoneConfig.RangeMaxBytes = proto.Int64(100 << 20)
	defaultZoneConfig.GC = &zonepb.GCPolicy{TTLSeconds: 60}
	require.NoError(t, defaultZoneConfig.Validate())
	params.Knobs.Server = &server.TestingKnobs{
		DefaultZoneConfigOverride:       &defaultZoneConfig,
		DefaultSystemZoneConfigOverride: &defaultZoneConfig,
	}

	s, sqlDB, _ := serverutils.StartServer(t, params)
	defer s.Stopper().Stop(context.Background())
	// Set the closed_timestamp interval to be short to shorten the test duration.
	systemDB := sqlutils.MakeSQLRunner(s.SystemLayer().SQLConn(t))
	systemDB.Exec(t, `SET CLUSTER SETTING kv.closed_timestamp.target_duration = '20ms'`)
	systemDB.Exec(t, `SET CLUSTER SETTING kv.closed_timestamp.side_transport_interval = '20ms'`)
	systemDB.Exec(t, `SET CLUSTER SETTING kv.rangefeed.closed_timestamp_refresh_interval = '20ms'`)

	codec := s.Codec()

	type testCase struct {
		objectID uint32

		// keySuffix and partitionName must specify the same subzone.
		keySuffix     []byte
		partitionName string

		zoneCfg zonepb.ZoneConfig
	}
	verifyZoneConfigs := func(testCases []testCase) {
		cfg := forceNewConfig(t, s)

		for tcNum, tc := range testCases {
			// Verify SystemConfig.GetZoneConfigForKey. GetZoneConfigForKey is
			// exclusive to the system tenant.
			if !s.StartedDefaultTestTenant() {
				key := append(roachpb.RKey(codec.TablePrefix(tc.objectID)), tc.keySuffix...)
				_, zoneCfg, err := config.TestingGetSystemTenantZoneConfigForKey(cfg, key) // Complete ZoneConfig
				if err != nil {
					t.Fatalf("#%d: err=%s", tcNum, err)
				}

				if !tc.zoneCfg.Equal(zoneCfg) {
					t.Errorf("#%d: bad zone config.\nexpected: %+v\ngot: %+v", tcNum, &tc.zoneCfg, zoneCfg)
				}
			}

			// Verify sql.GetZoneConfigInTxn.
			dummyIndex := systemschema.CommentsTable.GetPrimaryIndex()
			if err := sqltestutils.TestingDescsTxn(context.Background(), s, func(ctx context.Context, txn isql.Txn, col *descs.Collection) error {
				_, zoneCfg, subzone, err := sql.GetZoneConfigInTxn(
					ctx, txn.KV(), col, descpb.ID(tc.objectID), dummyIndex, tc.partitionName, false,
				)
				if err != nil {
					return err
				} else if subzone != nil {
					zoneCfg = &subzone.Config
				}
				if !tc.zoneCfg.Equal(zoneCfg) {
					t.Errorf("#%d: bad zone config.\nexpected: %+v\ngot: %+v", tcNum, &tc.zoneCfg, zoneCfg)
				}
				return nil

			}); err != nil {
				t.Fatalf("#%d: err=%s", tcNum, err)
			}
		}
	}

	{
		buf, err := protoutil.Marshal(&defaultZoneConfig)
		if err != nil {
			t.Fatal(err)
		}
		objID := keys.RootNamespaceID
		if _, err = sqlDB.Exec(`UPDATE system.zones SET config = $2 WHERE id = $1`, objID, buf); err != nil {
			t.Fatalf("problem writing zone %+v: %s", defaultZoneConfig, err)
		}
	}

	// Naming scheme for database and tables:
	// db1 has tables tb11 and tb12
	// db2 has tables tb21 and tb22

	if _, err := sqlDB.Exec(`CREATE DATABASE db1`); err != nil {
		t.Fatal(err)
	}

	if _, err := sqlDB.Exec(`CREATE DATABASE db2`); err != nil {
		t.Fatal(err)
	}

	if _, err := sqlDB.Exec(`CREATE TABLE db1.tb1 (k INT PRIMARY KEY, v INT)`); err != nil {
		t.Fatal(err)
	}

	if _, err := sqlDB.Exec(`CREATE TABLE db1.tb2 (k INT PRIMARY KEY, v INT)`); err != nil {
		t.Fatal(err)
	}

	if _, err := sqlDB.Exec(`CREATE TABLE db2.tb1 (k INT PRIMARY KEY, v INT)`); err != nil {
		t.Fatal(err)
	}

	if _, err := sqlDB.Exec(`CREATE TABLE db2.tb2 (k INT PRIMARY KEY, v INT)`); err != nil {
		t.Fatal(err)
	}

	if _, err := sqlDB.Exec(`TRUNCATE TABLE db2.tb2`); err != nil {
		t.Fatal(err)
	}

	db1 := sqlutils.QueryDatabaseID(t, sqlDB, "db1")
	db2 := sqlutils.QueryDatabaseID(t, sqlDB, "db2")
	tb11 := sqlutils.QueryTableID(t, sqlDB, "db1", "public", "tb1")
	tb12 := sqlutils.QueryTableID(t, sqlDB, "db1", "public", "tb2")
	tb21 := sqlutils.QueryTableID(t, sqlDB, "db2", "public", "tb1")
	tb22 := sqlutils.QueryTableID(t, sqlDB, "db2", "public", "tb2")

	t.Logf("Verifying with no custom zone configs")
	verifyZoneConfigs([]testCase{
		{keys.RootNamespaceID, nil, "", defaultZoneConfig},
		{keys.SystemDatabaseID, nil, "", defaultZoneConfig},
		{db1, nil, "", defaultZoneConfig},
		{db2, nil, "", defaultZoneConfig},
		{tb11, nil, "", defaultZoneConfig},
		{tb11, []byte{42}, "p0", defaultZoneConfig},
		{tb12, nil, "", defaultZoneConfig},
		{tb12, []byte{42}, "p0", defaultZoneConfig},
		{tb21, nil, "", defaultZoneConfig},
		{tb22, nil, "", defaultZoneConfig},
	})

	// Now set some zone configs. We don't have a nice way of using table
	// names for this, so we do raw puts.
	// .default: has replciation factor of 1
	// db1: has replication factor of 5
	//   tb1: inherits replication factor from db1
	//   tb2: no zone config
	// db2: no zone config
	//   tb1: inherits replication factor from default
	//     p1: true [1, 2), [6, 7) - Explicitly set replciation factor
	//     p2: true [3, 5) - inherits repliaction factor from default
	//   tb2: no zone config
	//     p1: true  [1, 255) - inherits replciation factor from default

	makeConstraints := func(value string) []zonepb.ConstraintsConjunction {
		return []zonepb.ConstraintsConjunction{
			{
				Constraints: []zonepb.Constraint{
					{Type: zonepb.Constraint_REQUIRED, Value: value},
				},
			},
		}
	}
	db1Cfg := *zonepb.NewZoneConfig()
	db1Cfg.NumReplicas = proto.Int32(5)
	db1Cfg.Constraints = makeConstraints("db1")
	db1Cfg.InheritedConstraints = false

	// Expected complete config
	expectedDb1Cfg := defaultZoneConfig
	expectedDb1Cfg.NumReplicas = proto.Int32(5)
	expectedDb1Cfg.Constraints = makeConstraints("db1")

	tb11Cfg := *zonepb.NewZoneConfig()
	tb11Cfg.Constraints = makeConstraints("db1.tb1")
	tb11Cfg.InheritedConstraints = false

	// Expected complete config
	expectedTb11Cfg := expectedDb1Cfg
	expectedTb11Cfg.Constraints = makeConstraints("db1.tb1")

	p211Cfg := *zonepb.NewZoneConfig()
	p211Cfg.NumReplicas = proto.Int32(1)
	p211Cfg.Constraints = makeConstraints("db2.tb1.p1")
	p211Cfg.InheritedConstraints = false

	// Expected complete config
	expectedP211Cfg := defaultZoneConfig
	expectedP211Cfg.NumReplicas = proto.Int32(1)
	expectedP211Cfg.Constraints = makeConstraints("db2.tb1.p1")

	p212Cfg := *zonepb.NewZoneConfig()
	p212Cfg.Constraints = makeConstraints("db2.tb1.p2")
	p212Cfg.InheritedConstraints = false

	// Expected complete config
	expectedP212Cfg := defaultZoneConfig
	expectedP212Cfg.Constraints = makeConstraints("db2.tb1.p2")

	tb21Cfg := *zonepb.NewZoneConfig()
	tb21Cfg.Constraints = makeConstraints("db2.tb1")
	tb21Cfg.InheritedConstraints = false
	tb21Cfg.Subzones = []zonepb.Subzone{
		{IndexID: 1, PartitionName: "p0", Config: p211Cfg},
		{IndexID: 1, PartitionName: "p1", Config: p212Cfg},
	}
	tb21Cfg.SubzoneSpans = []zonepb.SubzoneSpan{
		{SubzoneIndex: 0, Key: []byte{1}},
		{SubzoneIndex: 1, Key: []byte{3}, EndKey: []byte{5}},
		{SubzoneIndex: 0, Key: []byte{6}},
	}

	// Expected complete config
	expectedTb21Cfg := defaultZoneConfig
	expectedTb21Cfg.Constraints = makeConstraints("db2.tb1")
	expectedTb21Cfg.Subzones = []zonepb.Subzone{
		{IndexID: 1, PartitionName: "p0", Config: p211Cfg},
		{IndexID: 1, PartitionName: "p1", Config: p212Cfg},
	}
	expectedTb21Cfg.SubzoneSpans = []zonepb.SubzoneSpan{
		{SubzoneIndex: 0, Key: []byte{1}},
		{SubzoneIndex: 1, Key: []byte{3}, EndKey: []byte{5}},
		{SubzoneIndex: 0, Key: []byte{6}},
	}

	p221Cfg := *zonepb.NewZoneConfig()
	p221Cfg.Constraints = makeConstraints("db2.tb2.p1")
	p221Cfg.InheritedConstraints = false

	// Expected complete config
	expectedP221Cfg := defaultZoneConfig
	expectedP221Cfg.Constraints = makeConstraints("db2.tb2.p1")

	// Subzone Placeholder
	tb22Cfg := *zonepb.NewZoneConfig()
	tb22Cfg.NumReplicas = proto.Int32(0)
	tb22Cfg.Subzones = []zonepb.Subzone{{IndexID: 1, PartitionName: "p0", Config: p221Cfg}}
	tb22Cfg.SubzoneSpans = []zonepb.SubzoneSpan{
		{SubzoneIndex: 0, Key: []byte{1}, EndKey: []byte{255}},
	}

	for objID, objZone := range map[uint32]zonepb.ZoneConfig{
		db1:  db1Cfg,
		tb11: tb11Cfg,
		tb21: tb21Cfg,
		tb22: tb22Cfg,
	} {
		buf, err := protoutil.Marshal(&objZone)
		if err != nil {
			t.Fatal(err)
		}
		if _, err = sqlDB.Exec(`INSERT INTO system.zones VALUES ($1, $2)`, objID, buf); err != nil {
			t.Fatalf("problem writing zone %+v: %s", objZone, err)
		}
	}

	t.Logf("Verifying with custom zone configs")
	verifyZoneConfigs([]testCase{
		{keys.RootNamespaceID, nil, "", defaultZoneConfig},
		{keys.SystemDatabaseID, nil, "", defaultZoneConfig},
		{db1, nil, "", expectedDb1Cfg},
		{db2, nil, "", defaultZoneConfig},
		{tb11, nil, "", expectedTb11Cfg},
		{tb11, []byte{42}, "p0", expectedTb11Cfg},
		{tb12, nil, "", expectedDb1Cfg},
		{tb12, []byte{42}, "p0", expectedDb1Cfg},
		{tb21, nil, "", expectedTb21Cfg},
		{tb21, []byte{}, "", expectedTb21Cfg},
		{tb21, []byte{0}, "", expectedTb21Cfg},
		{tb21, []byte{1}, "p0", expectedP211Cfg},
		{tb21, []byte{1, 255}, "p0", expectedP211Cfg},
		{tb21, []byte{2}, "", expectedTb21Cfg},
		{tb21, []byte{3}, "p1", expectedP212Cfg},
		{tb21, []byte{4}, "p1", expectedP212Cfg},
		{tb21, []byte{5}, "", expectedTb21Cfg},
		{tb21, []byte{6}, "p0", expectedP211Cfg},
		{tb22, nil, "", defaultZoneConfig},
		{tb22, []byte{0}, "", defaultZoneConfig},
		{tb22, []byte{1}, "p0", expectedP221Cfg},
		{tb22, []byte{255}, "", defaultZoneConfig},
	})

	// Change the default.
	defaultZoneConfig.NumReplicas = proto.Int32(5)

	buf, err := protoutil.Marshal(&defaultZoneConfig)
	if err != nil {
		t.Fatal(err)
	}
	objID := keys.RootNamespaceID
	if _, err = sqlDB.Exec(`UPDATE system.zones SET config = $2 WHERE id = $1`, objID, buf); err != nil {
		t.Fatalf("problem writing zone %+v: %s", defaultZoneConfig, err)
	}

	// Ensure the changes cascade down.
	expectedTb21Cfg.NumReplicas = proto.Int32(5)
	expectedP212Cfg.NumReplicas = proto.Int32(5)
	expectedP221Cfg.NumReplicas = proto.Int32(5)

	verifyZoneConfigs([]testCase{
		// TODO(ridwanmsharif): Figure out what these 3 are supposed to do.
		// {0, nil, "", defaultZoneConfig},
		// {1, nil, "", cfg},
		// {keys.MaxReservedDescID, nil, "", defaultZoneConfig},
		{db1, nil, "", expectedDb1Cfg},
		{db2, nil, "", defaultZoneConfig},
		{tb11, nil, "", expectedTb11Cfg},
		{tb11, []byte{42}, "p0", expectedTb11Cfg},
		{tb12, nil, "", expectedDb1Cfg},
		{tb12, []byte{42}, "p0", expectedDb1Cfg},
		{tb21, nil, "", expectedTb21Cfg},
		{tb21, []byte{}, "", expectedTb21Cfg},
		{tb21, []byte{0}, "", expectedTb21Cfg},
		{tb21, []byte{1}, "p0", expectedP211Cfg},
		{tb21, []byte{1, 255}, "p0", expectedP211Cfg},
		{tb21, []byte{2}, "", expectedTb21Cfg},
		{tb21, []byte{3}, "p1", expectedP212Cfg},
		{tb21, []byte{4}, "p1", expectedP212Cfg},
		{tb21, []byte{5}, "", expectedTb21Cfg},
		{tb21, []byte{6}, "p0", expectedP211Cfg},
		{tb22, nil, "", defaultZoneConfig},
		{tb22, []byte{0}, "", defaultZoneConfig},
		{tb22, []byte{1}, "p0", expectedP221Cfg},
		{tb22, []byte{255}, "", defaultZoneConfig},
	})
}

func BenchmarkGetZoneConfig(b *testing.B) {
	defer leaktest.AfterTest(b)()
	defer log.Scope(b).Close(b)

	params, _ := createTestServerParamsAllowTenants()
	// GetZoneConfigForKey is exclusive to the system tenant.
	params.DefaultTestTenant = base.TestIsSpecificToStorageLayerAndNeedsASystemTenant
	s, sqlDB, _ := serverutils.StartServer(b, params)
	defer s.Stopper().Stop(context.Background())
	// Set the closed_timestamp interval to be short to shorten the test duration.
	tdb := sqlutils.MakeSQLRunner(sqlDB)
	tdb.Exec(b, `SET CLUSTER SETTING kv.closed_timestamp.target_duration = '20ms'`)
	tdb.Exec(b, `SET CLUSTER SETTING kv.closed_timestamp.side_transport_interval = '20ms'`)
	tdb.Exec(b, `SET CLUSTER SETTING kv.rangefeed.closed_timestamp_refresh_interval = '20ms'`)
	cfg := forceNewConfig(b, s)

	key := roachpb.RKey(s.Codec().TablePrefix(bootstrap.TestingUserDescID(0)))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, _, err := config.TestingGetSystemTenantZoneConfigForKey(cfg, key)
		if err != nil {
			b.Fatal(err)
		}
	}
	b.StopTimer()
}
