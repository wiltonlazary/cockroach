// Copyright 2022 The Cockroach Authors.
//
// Use of this software is governed by the Business Source License
// included in the file licenses/BSL.txt.
//
// As of the Change Date specified in that file, in accordance with
// the Business Source License, use of this software will be governed
// by the Apache License, Version 2.0, included in the file
// licenses/APL.txt.

package schematelemetry_test

import (
	"context"
	"testing"

	"github.com/cockroachdb/cockroach/pkg/base"
	"github.com/cockroachdb/cockroach/pkg/keys"
	"github.com/cockroachdb/cockroach/pkg/sql/catalog/desctestutils"
	"github.com/cockroachdb/cockroach/pkg/sql/catalog/schematelemetry"
	"github.com/cockroachdb/cockroach/pkg/sql/catalog/tabledesc"
	"github.com/cockroachdb/cockroach/pkg/testutils/serverutils"
	"github.com/cockroachdb/cockroach/pkg/testutils/sqlutils"
	"github.com/cockroachdb/cockroach/pkg/util/leaktest"
	"github.com/cockroachdb/cockroach/pkg/util/log"
	"github.com/stretchr/testify/require"
)

func TestRedactQueries(t *testing.T) {
	defer leaktest.AfterTest(t)()
	defer log.Scope(t).Close(t)

	ctx := context.Background()
	s, db, kvDB := serverutils.StartServer(t, base.TestServerArgs{})
	defer s.Stopper().Stop(ctx)
	tdb := sqlutils.MakeSQLRunner(db)
	tdb.Exec(t, "CREATE TABLE kv (k INT PRIMARY KEY, v STRING)")
	tdb.Exec(t, "CREATE VIEW view AS SELECT k, v FROM kv WHERE v <> 'constant literal'")
	tdb.Exec(t, "CREATE TABLE ctas AS SELECT k, v FROM kv WHERE v <> 'constant literal'")

	t.Run("view", func(t *testing.T) {
		view := desctestutils.TestingGetTableDescriptor(
			kvDB, keys.SystemSQLCodec, "defaultdb", "public", "view",
		)
		mut := tabledesc.NewBuilder(view.TableDesc()).BuildCreatedMutableTable()
		require.Empty(t, schematelemetry.Redact(mut))
		require.Equal(t, `SELECT k, v FROM defaultdb.public.kv WHERE v != '_'`, mut.ViewQuery)
	})

	t.Run("create table as", func(t *testing.T) {
		ctas := desctestutils.TestingGetTableDescriptor(
			kvDB, keys.SystemSQLCodec, "defaultdb", "public", "ctas",
		)
		mut := tabledesc.NewBuilder(ctas.TableDesc()).BuildCreatedMutableTable()
		require.Empty(t, schematelemetry.Redact(mut))
		require.Equal(t, `SELECT k, v FROM defaultdb.public.kv WHERE v != '_'`, mut.CreateQuery)
	})
}
