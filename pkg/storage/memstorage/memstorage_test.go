// Copyright 2026 JC-Lab
// SPDX-License-Identifier: EPL-2.0

package memstorage_test

import (
	"testing"

	"github.com/durupages/durupages/pkg/storage"
	"github.com/durupages/durupages/pkg/storage/memstorage"
	"github.com/durupages/durupages/pkg/storage/storagetest"
)

func TestConformance(t *testing.T) {
	storagetest.RunConformance(t, func(t *testing.T) storage.Storage {
		return memstorage.New()
	})
}
