// Copyright (c) DuruPages
// Licensed under the Apache 2.0 license.
//
// duru-limits.h — DuruPages IsolateLimitEnforcer.
//
// The stock workerd binary constructs a NullIsolateLimitEnforcer ("enforces no
// limits") for every worker isolate, so a page's V8 heap can grow unbounded.
// DuruIsolateLimitEnforcer replaces it with a real per-isolate heap limit,
// enforced at the V8 level (not via OS cgroups), so a runaway page worker is
// condemned instead of taking the whole tenant pod down via OOMKill.
//
// It is a drop-in for the same IsolateLimitEnforcer interface: every method
// that Null implemented as a no-op keeps that behaviour (the JS-entry scopes
// are startup/logging/inspector paths, not the request hot path), and the two
// methods that actually matter for a heap limit — getCreateParams(),
// customizeIsolate(), exitJs() and hasExcessivelyExceededHeapLimit() — are
// implemented for real.
//
// See docs/ARCHITECTURE.md 6.6 and native/durupages-workerd/README.md.

#pragma once

#include <workerd/io/limit-enforcer.h>
#include <workerd/io/tracked-wasm-instance.h>

#include <v8-isolate.h>

#include <atomic>

namespace workerd::durupages {

// HeapLimitConfig is read once at process start (from the environment) and
// applied to every isolate this process creates.
struct HeapLimitConfig {
  // maxOldGenBytes caps the V8 old-generation heap. 0 disables the limit
  // (falling back to V8 defaults, i.e. Null behaviour).
  size_t maxOldGenBytes = 0;

  // Reads DURUPAGES_ISOLATE_HEAP_LIMIT_MB from the environment; 0/absent/invalid
  // means "no limit".
  static HeapLimitConfig fromEnv();
};

// DuruIsolateLimitEnforcer enforces a heap limit on a single worker isolate.
// One instance is created per isolate (per page worker), so the condemned flag
// is per-isolate.
class DuruIsolateLimitEnforcer final: public IsolateLimitEnforcer {
 public:
  explicit DuruIsolateLimitEnforcer(HeapLimitConfig config): config(config) {}

  // Sets ResourceConstraints (old-generation cap) so V8 sizes the isolate for
  // the configured limit up front.
  v8::Isolate::CreateParams getCreateParams() override;

  // Installs a near-heap-limit callback that condemns the isolate when the heap
  // limit is approached, granting a one-time bump so the in-flight request can
  // unwind cleanly before the isolate is torn down.
  void customizeIsolate(v8::Isolate* isolate) override;

  ActorCacheSharedLruOptions getActorCacheLruOptions() override;

  // JS-entry scopes: startup / dynamic import / logging / inspector are not the
  // request hot path; keep Null's no-limit behaviour (return an empty scope).
  kj::Own<void> enterStartupJs(
      jsg::Lock& lock, kj::OneOf<kj::Exception, kj::Duration>&) const override {
    return {};
  }
  kj::Own<void> enterStartupPython(
      jsg::Lock& lock, kj::OneOf<kj::Exception, kj::Duration>&) const override {
    return {};
  }
  kj::Own<void> enterDynamicImportJs(
      jsg::Lock& lock, kj::OneOf<kj::Exception, kj::Duration>&) const override {
    return {};
  }
  kj::Own<void> enterLoggingJs(
      jsg::Lock& lock, kj::OneOf<kj::Exception, kj::Duration>&) const override {
    return {};
  }
  kj::Own<void> enterInspectorJs(
      jsg::Lock& lock, kj::OneOf<kj::Exception, kj::Duration>&) const override {
    return {};
  }

  void completedRequest(kj::StringPtr id) const override {}

  // Called when releasing the isolate lock. Returns true if the isolate is
  // condemned (heap limit exceeded), which tears the isolate down.
  bool exitJs(jsg::Lock& lock) const override;

  void reportMetrics(IsolateObserver& isolateMetrics) const override {}

  bool hasExcessivelyExceededHeapLimit() const override {
    return condemned.load(std::memory_order_relaxed);
  }

  const TrackedWasmInstanceList& getTrackedWasmInstances() const override {
    return trackedWasmInstances;
  }

 private:
  HeapLimitConfig config;
  TrackedWasmInstanceList trackedWasmInstances;

  // The isolate this enforcer guards, captured in customizeIsolate(). Used by
  // the near-heap-limit callback to terminate the offending execution.
  v8::Isolate* isolate = nullptr;

  // Set once the heap limit has been breached. Atomic because the near-heap
  // callback may run on V8's GC path while exitJs()/hasExcessively... read it.
  mutable std::atomic<bool> condemned{false};

  // V8 near-heap-limit callback. `data` is the owning enforcer.
  //
  // V8's contract: the returned value MUST be strictly greater than
  // currentHeapLimit, otherwise V8 declares a fatal OOM and aborts the whole
  // process. So we always grant headroom (avoiding the abort) and instead stop
  // the runaway by calling Isolate::TerminateExecution(), which unwinds the
  // offending request with an uncatchable termination while leaving the process
  // — and every other tenant's isolate in it — alive. The isolate is condemned
  // and torn down afterwards.
  static size_t nearHeapLimitCallback(
      void* data, size_t currentHeapLimit, size_t initialHeapLimit);
};

}  // namespace workerd::durupages
