// Copyright (c) DuruPages
// Licensed under the Apache 2.0 license.

#include "duru-limits.h"

#include <workerd/io/actor-cache.h>

#include <kj/debug.h>

#include <cstdlib>

namespace workerd::durupages {

HeapLimitConfig HeapLimitConfig::fromEnv() {
  HeapLimitConfig cfg;
  if (const char* mb = getenv("DURUPAGES_ISOLATE_HEAP_LIMIT_MB")) {
    char* end = nullptr;
    unsigned long long v = strtoull(mb, &end, 10);
    if (end != mb && *end == '\0' && v > 0) {
      cfg.maxOldGenBytes = static_cast<size_t>(v) * 1024 * 1024;
    }
  }
  return cfg;
}

v8::Isolate::CreateParams DuruIsolateLimitEnforcer::getCreateParams() {
  v8::Isolate::CreateParams params;
  if (config.maxOldGenBytes > 0) {
    // Size the isolate for the configured cap up front. The young generation is
    // left at V8's default; only the old generation (where a runaway heap grows)
    // is bounded, matching how a request's live set is accounted.
    params.constraints.set_max_old_generation_size_in_bytes(config.maxOldGenBytes);
  }
  return params;
}

void DuruIsolateLimitEnforcer::customizeIsolate(v8::Isolate* isolate) {
  if (config.maxOldGenBytes == 0) return;
  this->isolate = isolate;
  // Fire when the heap approaches the configured cap. The callback terminates
  // the offending execution rather than letting V8 fatally OOM.
  isolate->AddNearHeapLimitCallback(&DuruIsolateLimitEnforcer::nearHeapLimitCallback, this);
}

size_t DuruIsolateLimitEnforcer::nearHeapLimitCallback(
    void* data, size_t currentHeapLimit, size_t initialHeapLimit) {
  auto* self = static_cast<DuruIsolateLimitEnforcer*>(data);
  self->condemned.store(true, std::memory_order_relaxed);
  // Stop the runaway allocation with an uncatchable termination. This unwinds
  // the current request (workerd reports it as an exceeded-resources failure)
  // without aborting the process.
  if (self->isolate != nullptr) {
    self->isolate->TerminateExecution();
  }
  // Grant generous headroom so V8 does not declare a fatal OOM (which requires
  // returning a value <= currentHeapLimit) before the termination lands. Since
  // execution is terminating, this headroom is not actually consumed.
  return currentHeapLimit + (64u << 20);
}

bool DuruIsolateLimitEnforcer::exitJs(jsg::Lock& lock) const {
  // If the near-heap callback fired, the isolate is condemned: report it so the
  // supervisor tears the isolate down.
  return condemned.load(std::memory_order_relaxed);
}

ActorCacheSharedLruOptions DuruIsolateLimitEnforcer::getActorCacheLruOptions() {
  // Mirror workerd's NullIsolateLimitEnforcer defaults: in-memory-only actors.
  return {
    .softLimit = 16 * (1ull << 20),  // 16 MiB
    .hardLimit = 128 * (1ull << 20),  // 128 MiB
    .staleTimeout = 30 * kj::SECONDS,
    .dirtyListByteLimit = 8 * (1ull << 20),  // 8 MiB
    .maxKeysPerRpc = 128,
    .neverFlush = true,
  };
}

}  // namespace workerd::durupages
