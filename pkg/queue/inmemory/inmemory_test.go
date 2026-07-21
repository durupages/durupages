// Copyright 2026 JC-Lab
// SPDX-License-Identifier: EPL-2.0

package inmemory_test

import (
	"testing"

	"github.com/durupages/durupages/pkg/queue"
	"github.com/durupages/durupages/pkg/queue/inmemory"
	"github.com/durupages/durupages/pkg/queue/queuetest"
)

func TestConformance(t *testing.T) {
	queuetest.RunConformance(t, func(t *testing.T) queue.Queue {
		return inmemory.New()
	})
}
