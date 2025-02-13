// Copyright 2021 The Cockroach Authors.
//
// Use of this software is governed by the Business Source License
// included in the file licenses/BSL.txt.
//
// As of the Change Date specified in that file, in accordance with
// the Business Source License, use of this software will be governed
// by the Apache License, Version 2.0, included in the file
// licenses/APL.txt.

package opgen

import (
	"github.com/cockroachdb/cockroach/pkg/sql/schemachanger/scop"
	"github.com/cockroachdb/cockroach/pkg/sql/schemachanger/scpb"
)

func init() {
	opRegistry.register((*scpb.ConstraintComment)(nil),
		toPublic(
			scpb.Status_ABSENT,
			to(scpb.Status_PUBLIC,
				emit(func(this *scpb.ConstraintComment) *scop.UpsertConstraintComment {
					return &scop.UpsertConstraintComment{
						TableID:      this.TableID,
						ConstraintID: this.ConstraintID,
						Comment:      this.Comment,
					}
				}),
				emit(func(this *scpb.ConstraintComment, md *targetsWithElementMap) *scop.LogEvent {
					return newLogEventOp(this, md)
				}),
			),
		),
		toAbsent(
			scpb.Status_PUBLIC,
			to(scpb.Status_ABSENT,
				emit(func(this *scpb.ConstraintComment) *scop.RemoveConstraintComment {
					return &scop.RemoveConstraintComment{
						TableID:      this.TableID,
						ConstraintID: this.ConstraintID,
					}
				}),
				emit(func(this *scpb.ConstraintComment, md *targetsWithElementMap) *scop.LogEvent {
					return newLogEventOp(this, md)
				}),
			),
		),
	)
}
