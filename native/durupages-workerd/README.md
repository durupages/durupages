# durupages-workerd

workerd 를 patch 하여 빌드한 커스텀 바이너리입니다. **공식 workerd 바이너리는 리소스 제한과
요청별 CPU 계측이 모두 비활성**임을 소스 분석과 실측으로 확인했기 때문에 필수입니다
(근거: [docs/ARCHITECTURE.md 6.6](../../docs/ARCHITECTURE.md)).

빌드 산출물은 stock workerd 와 CLI 가 동일한 `durupages-workerd` 바이너리이며, shim 은
`DURUPAGES_WORKERD_BIN` 으로 이 경로를 가리키면 됩니다.

## 구성

pinned workerd 리비전([WORKERD_VERSION](WORKERD_VERSION))을 clone 한 뒤 [patches/](patches/) 의
패치를 적용해 빌드합니다. workerd 를 fork 하지 않고 **최소 패치 + 추가 소스 파일**로 구성해
버전업 추적 비용을 낮춥니다.

```
native/durupages-workerd/
├── WORKERD_VERSION      # pinned workerd ref
├── build.sh             # clone → patch → bazel build → bin/durupages-workerd
├── patches/
│   └── 0001-duru-isolate-heap-limit.patch
└── src/                 # 패치가 workerd 트리에 추가하는 소스 (리뷰용 canonical 사본)
    ├── duru-limits.h
    └── duru-limits.c++
```

## 구현 현황

| 대상 | 상태 | 내용 |
|---|---|---|
| `NullIsolateLimitEnforcer` → `DuruIsolateLimitEnforcer` | **구현·실측 완료** | isolate(page worker) 별 **V8 old-generation heap limit**. `getCreateParams()` 로 `ResourceConstraints` 설정, `customizeIsolate()` 로 near-heap-limit 콜백 설치. 한도 근접 시 `Isolate::TerminateExecution()` 으로 해당 요청만 중단하고 isolate 를 condemn → **프로세스 abort 없이** 요청만 실패, isolate 폐기(pod OOMKill·타 테넌트 영향 회피). 한도는 `DURUPAGES_ISOLATE_HEAP_LIMIT_MB` (0/미설정 = stock no-limit) |
| 요청 수준 `LimitEnforcer` (CPU time) | **미구현 (다음 패치)** | `enterJs()`/`exitJs()` 의 `CLOCK_THREAD_CPUTIME_ID` 델타를 IoContext 별 누적, page CPU 한도 초과 시 `getLimitsExceeded()` 로 중단 |
| `RequestObserverWithTracer` cpuTime/wallTime | **미구현 (다음 패치)** | 요청 종료 시 실측값으로 `WorkerTracer::setOutcome()` 호출 → tail 이벤트에 실제 cpuTime 전달 |

heap limit 은 그 자체로 완결적이며(요청 수준 CPU 계측과 독립), "리소스 제한이 전혀 없다"는
문제 중 메모리 부분을 workerd(V8) 수준에서 해결합니다. CPU 계측/제한은 IoContext ↔ observer
상관(correlation)이 필요한 더 큰 패치로, 후속 작업입니다.

**실측 (workerd 2026-07-21, 이 저장소 빌드):** old-gen 을 채우는 worker 로 검증.
- `DURUPAGES_ISOLATE_HEAP_LIMIT_MB` 미설정 → 5,000,000개 객체 할당 성공 (HTTP 200, stock 동작).
- `=64` → 힙이 ~79MB 에서 억제되고 요청은 `script terminated` 로 실패 (HTTP 500), **workerd 프로세스는 생존**(abort/fatal 0건). 이후 요청도 정상 서빙.

  즉 near-heap 콜백이 한도 고정값을 반환하면 V8 가 fatal OOM → 프로세스 abort 하므로(멀티테넌트에
  치명적), 대신 `TerminateExecution()` + 넉넉한 headroom 으로 "요청만 실패 + isolate 폐기 + 프로세스
  생존" 을 달성합니다.

## 빌드

요구사항: git, C++20 clang 툴체인(clang-20 등), `tclsh`(sqlite 빌드용), bazelisk/bazel.
`build.sh` 가 버전 있는 이름(`clang-20`/`tclsh8.6` 등)을 자동으로 심링크하므로 stock Ubuntu 에서
바로 동작합니다.

```sh
cd native/durupages-workerd
./build.sh          # → bin/durupages-workerd
```

- V8 를 소스에서 빌드하므로 최초 빌드는 오래 걸립니다(수십 분). `--disk_cache` 로 재빌드는 캐시를
  재사용합니다. 메모리 사용량이 커서 `DURUPAGES_BUILD_JOBS` / `DURUPAGES_BUILD_MEM_MB` 로 조절합니다.
- 산출물은 `Dockerfile` 의 worker 이미지에 넣어 사용합니다(현재는 dev fallback 으로 공식 workerd 를
  번들). CI 의 worker 이미지 릴리스는 이 바이너리가 준비되면 활성화합니다.

## 개발 단계 폴백

durupages-workerd 를 아직 배포하지 않은 환경에서 shim 은 공식 workerd 바이너리로도 동작합니다
(`DURUPAGES_WORKERD_BIN`). 이 경우:

- tail 이벤트의 cpuTime/wallTime 이 0 → `RequestUsage.CPUTime` 이 0 으로 기록됩니다 (과금 불가).
- isolate heap 제한이 걸리지 않습니다 → pod resources limit 만으로 제한됩니다.

durupages-workerd 를 쓰면 heap limit 이 활성화되고(위 표), CPU 계측은 후속 패치 적용 후 활성화됩니다.
