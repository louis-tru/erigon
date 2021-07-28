/*
   Copyright 2021 Erigon contributors

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

package mdbx_test

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/ledgerwatch/erigon-lib/gointerfaces"
	"github.com/ledgerwatch/erigon-lib/gointerfaces/remote"
	"github.com/ledgerwatch/erigon-lib/kv"
	"github.com/ledgerwatch/erigon-lib/kv/mdbx"
	"github.com/ledgerwatch/erigon-lib/kv/remotedb"
	"github.com/ledgerwatch/erigon-lib/kv/remotedbserver"
	"github.com/ledgerwatch/erigon-lib/log"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/test/bufconn"
)

func TestSequence(t *testing.T) {
	writeDBs, _ := setupDatabases(t, log.New(), func(defaultBuckets kv.TableCfg) kv.TableCfg {
		return defaultBuckets
	})
	ctx := context.Background()

	for _, db := range writeDBs {
		db := db
		tx, err := db.BeginRw(ctx)
		require.NoError(t, err)
		defer tx.Rollback()

		i, err := tx.ReadSequence(kv.ChaindataTables[0])
		require.NoError(t, err)
		require.Equal(t, uint64(0), i)
		i, err = tx.IncrementSequence(kv.ChaindataTables[0], 1)
		require.NoError(t, err)
		require.Equal(t, uint64(0), i)
		i, err = tx.IncrementSequence(kv.ChaindataTables[0], 6)
		require.NoError(t, err)
		require.Equal(t, uint64(1), i)
		i, err = tx.IncrementSequence(kv.ChaindataTables[0], 1)
		require.NoError(t, err)
		require.Equal(t, uint64(7), i)

		i, err = tx.ReadSequence(kv.ChaindataTables[1])
		require.NoError(t, err)
		require.Equal(t, uint64(0), i)
		i, err = tx.IncrementSequence(kv.ChaindataTables[1], 1)
		require.NoError(t, err)
		require.Equal(t, uint64(0), i)
		i, err = tx.IncrementSequence(kv.ChaindataTables[1], 6)
		require.NoError(t, err)
		require.Equal(t, uint64(1), i)
		i, err = tx.IncrementSequence(kv.ChaindataTables[1], 1)
		require.NoError(t, err)
		require.Equal(t, uint64(7), i)
		tx.Rollback()
	}
}

func TestManagedTx(t *testing.T) {
	logger := log.NewTest(t)
	defaultConfig := kv.ChaindataTablesCfg
	defer func() {
		kv.ChaindataTablesCfg = defaultConfig
	}()

	bucketID := 0
	bucket1 := kv.ChaindataTables[bucketID]
	bucket2 := kv.ChaindataTables[bucketID+1]
	writeDBs, readDBs := setupDatabases(t, logger, func(defaultBuckets kv.TableCfg) kv.TableCfg {
		return map[string]kv.TableCfgItem{
			bucket1: {
				Flags:                     kv.DupSort,
				AutoDupSortKeysConversion: true,
				DupToLen:                  4,
				DupFromLen:                6,
			},
			bucket2: {
				Flags: 0,
			},
		}
	})

	ctx := context.Background()

	for _, db := range writeDBs {
		db := db
		tx, err := db.BeginRw(ctx)
		require.NoError(t, err)
		defer tx.Rollback()

		c, err := tx.RwCursor(bucket1)
		require.NoError(t, err)
		c1, err := tx.RwCursor(bucket2)
		require.NoError(t, err)
		require.NoError(t, c.Append([]byte{0}, []byte{1}))
		require.NoError(t, c1.Append([]byte{0}, []byte{1}))
		require.NoError(t, c.Append([]byte{0, 0, 0, 0, 0, 1}, []byte{1})) // prefixes of len=FromLen for DupSort test (other keys must be <ToLen)
		require.NoError(t, c1.Append([]byte{0, 0, 0, 0, 0, 1}, []byte{1}))
		require.NoError(t, c.Append([]byte{0, 0, 0, 0, 0, 2}, []byte{1}))
		require.NoError(t, c1.Append([]byte{0, 0, 0, 0, 0, 2}, []byte{1}))
		require.NoError(t, c.Append([]byte{0, 0, 1}, []byte{1}))
		require.NoError(t, c1.Append([]byte{0, 0, 1}, []byte{1}))
		for i := uint8(1); i < 10; i++ {
			require.NoError(t, c.Append([]byte{i}, []byte{1}))
			require.NoError(t, c1.Append([]byte{i}, []byte{1}))
		}
		require.NoError(t, c.Put([]byte{0, 0, 0, 0, 0, 1}, []byte{2}))
		require.NoError(t, c1.Put([]byte{0, 0, 0, 0, 0, 1}, []byte{2}))
		err = tx.Commit()
		require.NoError(t, err)
	}

	for _, db := range readDBs {
		db := db
		msg := fmt.Sprintf("%T", db)
		switch db.(type) {
		case *remotedb.RemoteKV:
		default:
			continue
		}

		t.Run("ctx cancel "+msg, func(t *testing.T) {
			t.Skip("probably need enable after go 1.4")
			testCtxCancel(t, db, bucket1)
		})
		t.Run("filter "+msg, func(t *testing.T) {
			//testPrefixFilter(t, db, bucket1)
		})
		t.Run("multiple cursors "+msg, func(t *testing.T) {
			testMultiCursor(t, db, bucket1, bucket2)
		})
	}
}

func TestRemoteKvVersion(t *testing.T) {
	logger := log.New()
	f := func(defaultBuckets kv.TableCfg) kv.TableCfg {
		return defaultBuckets
	}
	writeDb := mdbx.NewMDBX(logger).InMem().WithTablessCfg(f).MustOpen()
	defer writeDb.Close()
	conn := bufconn.Listen(1024 * 1024)
	grpcServer := grpc.NewServer()
	go func() {
		remote.RegisterKVServer(grpcServer, remotedbserver.NewKvServer(writeDb))
		if err := grpcServer.Serve(conn); err != nil {
			logger.Error("private RPC server fail", "err", err)
		}
	}()
	v := gointerfaces.VersionFromProto(remotedbserver.KvServiceAPIVersion)
	// Different Major versions
	v1 := v
	v1.Major++
	a, err := remotedb.NewRemote(v1, logger).InMem(conn).Open("", "", "")
	if err != nil {
		t.Fatalf("%v", err)
	}
	require.False(t, a.EnsureVersionCompatibility())
	// Different Minor versions
	v2 := v
	v2.Minor++
	_, err = remotedb.NewRemote(v2, logger).InMem(conn).Open("", "", "")
	if err != nil {
		t.Fatalf("%v", err)
	}
	require.False(t, a.EnsureVersionCompatibility())
	// Different Patch versions
	v3 := v
	v3.Patch++
	_, err = remotedb.NewRemote(v3, logger).InMem(conn).Open("", "", "")
	if err != nil {
		t.Fatalf("%v", err)
	}
	require.False(t, a.EnsureVersionCompatibility())
}

func setupDatabases(t *testing.T, logger log.Logger, f mdbx.TableCfgFunc) (writeDBs []kv.RwDB, readDBs []kv.RwDB) {
	writeDBs = []kv.RwDB{
		mdbx.NewMDBX(logger).InMem().WithTablessCfg(f).MustOpen(),
		mdbx.NewMDBX(logger).InMem().WithTablessCfg(f).MustOpen(), // for remote db
	}

	conn := bufconn.Listen(1024 * 1024)

	grpcServer := grpc.NewServer()
	f2 := func() {
		remote.RegisterKVServer(grpcServer, remotedbserver.NewKvServer(writeDBs[1]))
		if err := grpcServer.Serve(conn); err != nil {
			logger.Errorf("private RPC server fail: %s", err)
		}
	}
	go f2()
	v := gointerfaces.VersionFromProto(remotedbserver.KvServiceAPIVersion)
	rdb := remotedb.NewRemote(v, logger).InMem(conn).MustOpen()
	readDBs = []kv.RwDB{
		writeDBs[0],
		writeDBs[1],
		rdb,
	}

	t.Cleanup(func() {
		grpcServer.Stop()

		if err := conn.Close(); err != nil {
			panic(err)
		}

		for _, db := range readDBs {
			db.Close()
		}

		for _, db := range writeDBs {
			db.Close()
		}

	})
	return writeDBs, readDBs
}

func testCtxCancel(t *testing.T, db kv.RwDB, bucket1 string) {
	assert := assert.New(t)
	cancelableCtx, cancel := context.WithTimeout(context.Background(), time.Microsecond)
	defer cancel()

	if err := db.View(cancelableCtx, func(tx kv.Tx) error {
		c, err := tx.Cursor(bucket1)
		if err != nil {
			return err
		}
		for {
			for k, _, err := c.First(); k != nil; k, _, err = c.Next() {
				if err != nil {
					return err
				}
			}
		}
	}); err != nil {
		assert.True(errors.Is(context.DeadlineExceeded, err))
	}
}

func testMultiCursor(t *testing.T, db kv.RwDB, bucket1, bucket2 string) {
	assert, ctx := assert.New(t), context.Background()

	if err := db.View(ctx, func(tx kv.Tx) error {
		c1, err := tx.Cursor(bucket1)
		if err != nil {
			return err
		}
		c2, err := tx.Cursor(bucket2)
		if err != nil {
			return err
		}

		k1, v1, err := c1.First()
		assert.NoError(err)
		k2, v2, err := c2.First()
		assert.NoError(err)
		assert.Equal(k1, k2)
		assert.Equal(v1, v2)

		k1, v1, err = c1.Next()
		assert.NoError(err)
		k2, v2, err = c2.Next()
		assert.NoError(err)
		assert.Equal(k1, k2)
		assert.Equal(v1, v2)

		k1, v1, err = c1.Seek([]byte{0})
		assert.NoError(err)
		k2, v2, err = c2.Seek([]byte{0})
		assert.NoError(err)
		assert.Equal(k1, k2)
		assert.Equal(v1, v2)

		k1, v1, err = c1.Seek([]byte{0, 0})
		assert.NoError(err)
		k2, v2, err = c2.Seek([]byte{0, 0})
		assert.NoError(err)
		assert.Equal(k1, k2)
		assert.Equal(v1, v2)

		k1, v1, err = c1.Seek([]byte{0, 0, 0, 0})
		assert.NoError(err)
		k2, v2, err = c2.Seek([]byte{0, 0, 0, 0})
		assert.NoError(err)
		assert.Equal(k1, k2)
		assert.Equal(v1, v2)

		k1, v1, err = c1.Next()
		assert.NoError(err)
		k2, v2, err = c2.Next()
		assert.NoError(err)
		assert.Equal(k1, k2)
		assert.Equal(v1, v2)

		k1, v1, err = c1.Seek([]byte{0})
		assert.NoError(err)
		k2, v2, err = c2.Seek([]byte{0})
		assert.NoError(err)
		assert.Equal(k1, k2)
		assert.Equal(v1, v2)

		k1, v1, err = c1.Seek([]byte{0, 0})
		assert.NoError(err)
		k2, v2, err = c2.Seek([]byte{0, 0})
		assert.NoError(err)
		assert.Equal(k1, k2)
		assert.Equal(v1, v2)

		k1, v1, err = c1.Seek([]byte{0, 0, 0, 0})
		assert.NoError(err)
		k2, v2, err = c2.Seek([]byte{0, 0, 0, 0})
		assert.NoError(err)
		assert.Equal(k1, k2)
		assert.Equal(v1, v2)

		k1, v1, err = c1.Next()
		assert.NoError(err)
		k2, v2, err = c2.Next()
		assert.NoError(err)
		assert.Equal(k1, k2)
		assert.Equal(v1, v2)
		k1, v1, err = c1.Seek([]byte{2})
		assert.NoError(err)
		k2, v2, err = c2.Seek([]byte{2})
		assert.NoError(err)
		assert.Equal(k1, k2)
		assert.Equal(v1, v2)

		return nil
	}); err != nil {
		assert.NoError(err)
	}
}

//func TestMultipleBuckets(t *testing.T) {
//	writeDBs, readDBs, closeAll := setupDatabases(ethdb.WithChaindataTables)
//	defer closeAll()
//
//	ctx := context.Background()
//
//	for _, db := range writeDBs {
//		db := db
//		msg := fmt.Sprintf("%T", db)
//		t.Run("FillBuckets "+msg, func(t *testing.T) {
//			if err := db.Update(ctx, func(tx ethdb.Tx) error {
//				c := tx.Cursor(dbutils.ChaindataTables[0])
//				for i := uint8(0); i < 10; i++ {
//					require.NoError(t, c.Put([]byte{i}, []byte{i}))
//				}
//				c2 := tx.Cursor(dbutils.ChaindataTables[1])
//				for i := uint8(0); i < 12; i++ {
//					require.NoError(t, c2.Put([]byte{i}, []byte{i}))
//				}
//
//				// delete from first bucket key 5, then will seek on it and expect to see key 6
//				if err := c.Delete([]byte{5}, nil); err != nil {
//					return err
//				}
//				// delete non-existing key
//				if err := c.Delete([]byte{6, 1}, nil); err != nil {
//					return err
//				}
//
//				return nil
//			}); err != nil {
//				require.NoError(t, err)
//			}
//		})
//	}
//
//	for _, db := range readDBs {
//		db := db
//		msg := fmt.Sprintf("%T", db)
//		t.Run("MultipleBuckets "+msg, func(t *testing.T) {
//			counter2, counter := 0, 0
//			var key, value []byte
//			err := db.View(ctx, func(tx ethdb.Tx) error {
//				c := tx.Cursor(dbutils.ChaindataTables[0])
//				for k, _, err := c.First(); k != nil; k, _, err = c.Next() {
//					if err != nil {
//						return err
//					}
//					counter++
//				}
//
//				c2 := tx.Cursor(dbutils.ChaindataTables[1])
//				for k, _, err := c2.First(); k != nil; k, _, err = c2.Next() {
//					if err != nil {
//						return err
//					}
//					counter2++
//				}
//
//				c3 := tx.Cursor(dbutils.ChaindataTables[0])
//				k, v, err := c3.Seek([]byte{5})
//				if err != nil {
//					return err
//				}
//				key = common.CopyBytes(k)
//				value = common.CopyBytes(v)
//
//				return nil
//			})
//			require.NoError(t, err)
//			assert.Equal(t, 9, counter)
//			assert.Equal(t, 12, counter2)
//			assert.Equal(t, []byte{6}, key)
//			assert.Equal(t, []byte{6}, value)
//		})
//	}
//}

//func TestReadAfterPut(t *testing.T) {
//	writeDBs, _, closeAll := setupDatabases(ethdb.WithChaindataTables)
//	defer closeAll()
//
//	ctx := context.Background()
//
//	for _, db := range writeDBs {
//		db := db
//		msg := fmt.Sprintf("%T", db)
//		t.Run("GetAfterPut "+msg, func(t *testing.T) {
//			if err := db.Update(ctx, func(tx ethdb.Tx) error {
//				c := tx.Cursor(dbutils.ChaindataTables[0])
//				for i := uint8(0); i < 10; i++ { // don't read in same loop to check that writes don't affect each other (for example by sharing bucket.prefix buffer)
//					require.NoError(t, c.Put([]byte{i}, []byte{i}))
//				}
//
//				for i := uint8(0); i < 10; i++ {
//					v, err := c.SeekExact([]byte{i})
//					require.NoError(t, err)
//					require.Equal(t, []byte{i}, v)
//				}
//
//				c2 := tx.Cursor(dbutils.ChaindataTables[1])
//				for i := uint8(0); i < 12; i++ {
//					require.NoError(t, c2.Put([]byte{i}, []byte{i}))
//				}
//
//				for i := uint8(0); i < 12; i++ {
//					v, err := c2.SeekExact([]byte{i})
//					require.NoError(t, err)
//					require.Equal(t, []byte{i}, v)
//				}
//
//				{
//					require.NoError(t, c2.Delete([]byte{5}, nil))
//					v, err := c2.SeekExact([]byte{5})
//					require.NoError(t, err)
//					require.Nil(t, v)
//
//					require.NoError(t, c2.Delete([]byte{255}, nil)) // delete non-existing key
//				}
//
//				return nil
//			}); err != nil {
//				require.NoError(t, err)
//			}
//		})
//
//		t.Run("cursor put and delete"+msg, func(t *testing.T) {
//			if err := db.Update(ctx, func(tx ethdb.Tx) error {
//				c3 := tx.Cursor(dbutils.ChaindataTables[2])
//				for i := uint8(0); i < 10; i++ { // don't read in same loop to check that writes don't affect each other (for example by sharing bucket.prefix buffer)
//					require.NoError(t, c3.Put([]byte{i}, []byte{i}))
//				}
//				for i := uint8(0); i < 10; i++ {
//					v, err := tx.GetOne(dbutils.ChaindataTables[2], []byte{i})
//					require.NoError(t, err)
//					require.Equal(t, []byte{i}, v)
//				}
//
//				require.NoError(t, c3.Delete([]byte{255}, nil)) // delete non-existing key
//				return nil
//			}); err != nil {
//				t.Error(err)
//			}
//
//			if err := db.Update(ctx, func(tx ethdb.Tx) error {
//				c3 := tx.Cursor(dbutils.ChaindataTables[2])
//				require.NoError(t, c3.Delete([]byte{5}, nil))
//				v, err := tx.GetOne(dbutils.ChaindataTables[2], []byte{5})
//				require.NoError(t, err)
//				require.Nil(t, v)
//				return nil
//			}); err != nil {
//				t.Error(err)
//			}
//		})
//	}
//}
