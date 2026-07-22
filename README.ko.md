# DuruPages

> [English](README.md) · 한국어

**DuruPages** 는 Cloudflare Pages 를 어디서든 self-hosting 할 수 있게 해주는 멀티테넌트 플랫폼입니다. 이름은 "(Cloudflare) Pages 를 **두루** 이용할 수 있게 해준다"는 뜻입니다.

Cloudflare 의 실제 JS 런타임인 [workerd](https://github.com/cloudflare/workerd) 를 그대로 사용하므로, **wrangler 로 빌드한 Pages 프로젝트를 수정 없이 그대로 배포**할 수 있습니다. 정적 자산 서빙과 SSR worker(Functions) 가 Cloudflare Pages 와 동일하게 동작합니다.

- 아키텍처 상세: [docs/ARCHITECTURE.md](docs/ARCHITECTURE.md)
- CLI 레퍼런스: [docs/cli.md](docs/cli.md)

## 왜 DuruPages 인가

- **Cloudflare Pages 호환** — `_worker.js`, `_routes.json`, `_redirects`, `_headers`, `env.ASSETS`, `404.html` 상향 탐색, SPA fallback, pretty URL 등 Pages 의 서빙 규칙을 그대로 구현합니다. 기존 빌드 산출물 디렉토리를 그대로 올리면 됩니다.
- **진짜 멀티테넌시** — worker 는 **tenant 단위 Pod** 로 격리되어 실행되며, 테넌트 간에는 Pod(프로세스·네트워크·파일시스템)를 절대 공유하지 않습니다.
- **빠른 cold start** — Pod 는 번들 없이 즉시 Ready 가 되고, 각 page 는 **첫 요청 시점에 lazy load** 됩니다. 그래서 cold start 시간이 테넌트가 가진 page 수와 무관합니다.
- **무중단 배포** — 새 배포가 반영될 때 Pod 를 재시작하지 않습니다. shim 이 새 번들을 받아 **workerd 프로세스만 blue-green 으로 교체**하고, 처리 중이던 요청은 구 프로세스에서 자연스럽게 끝납니다.
- **요청 단위 계측** — 요청마다 wallTime/CPU/로그/예외/응답 상태를 수집합니다. page 의 Secret 값이 로그에 섞이면 Pod 를 떠나기 전에 자동으로 마스킹됩니다.
- **인프라 비종속** — Storage, PageProvider(DB), Queue, Scaler, Runtime 이 모두 Go 인터페이스로 분리되어 있습니다. 기본 구현(S3 / PostgreSQL / in-memory / workerd)을 교체해 조립할 수 있습니다.

## 동작 방식

```
Client ──▶ durupages-router ──▶ durupages-controller ──▶ (k8s) durupages-worker
             static 직접 서빙        queue · lease · 스케일링        shim + workerd
                                                                        │
                                                    durupages-hub ◀─────┘
                                                 번들 배포 · 사용량/로그 수집
```

1. `{pageId}.{pagesDomain}` 또는 커스텀 도메인으로 요청이 들어옵니다.
2. **router** 가 호스트로 page 를 찾고, `_routes.json` 기준으로 정적/동적을 판별합니다. 정적 자산은 router 가 로컬 LRU 캐시에서 바로 서빙합니다.
3. 동적 요청이면 **controller** 에 slot 을 요청합니다. controller 는 테넌트의 worker Pod 를 필요 시 생성하고(오토스케일링), lease 를 발급합니다.
4. **worker** Pod 의 shim 이 해당 page 번들을 **hub** 에서 받아(캐시 미보유 시) workerd 로 실행하고 응답합니다.
5. shim/router 는 요청별 사용량·로그를 **hub** 로 보냅니다(비활성 시 Pod 로그로만 출력).

## 구성요소

| 바이너리 | 역할 |
|---|---|
| `durupages-controller` | Control plane. 요청 queue/lease, worker Pod 생명주기, 오토스케일링, reconcile |
| `durupages-router` | 외부 진입점. 호스트 라우팅, 정적 서빙(디스크 LRU 캐시), `_redirects`/`_headers`, 동적 요청 프록시 |
| `durupages-hub` | worker 지원. tenant 스코프 번들 배포(JWT 인가), 요청 사용량·로그 수집 |
| `durupages-worker-shim` | worker Pod 의 PID 1. lazy load, workerd graceful swap, LRU 축출, 계측 |
| `durupages-workerd` | 커스텀 workerd 임베더(C++). isolate 별 리소스 제한 실구현 — [native/durupages-workerd](native/durupages-workerd) |
| `duru` | 배포 CLI. 빌드 산출물 스캔 → 업로드 → active deployment 원자적 전환 |

## 빠른 시작

로컬에 전체 스택(k3s + PostgreSQL + MinIO + 모든 구성요소)을 띄우고 e2e 시나리오를 실행합니다.

```sh
make e2e        # 이미지 빌드 → 스택 기동 → 시나리오 검증 → 정리
```

스택만 띄우고 직접 배포해 보려면:

```sh
make e2e-up

go run ./cmd/duru deploy \
  --dir ./e2e/fixtures/worker-site \
  --tenant acme --page app \
  --pg-dsn 'postgres://duru:duru@localhost:55432/duru?sslmode=disable' \
  --s3-endpoint http://localhost:59000 --s3-bucket durupages \
  --s3-access-key minioadmin --s3-secret-key minioadmin

curl -H 'Host: app.pages.local' http://localhost:18080/api/hello
```

배포 대상 디렉토리는 **Cloudflare Pages 에 올리는 빌드 산출물 그대로**입니다(`functions/` 를 쓴다면 `wrangler pages functions build` 로 먼저 컴파일하세요).

## 페이지 배포하기

`duru deploy` 에는 두 가지 모드가 있습니다. 하는 일(빌드 산출물 스캔 → 번들 업로드 → deployment 등록 → 활성화)은 같고, 클라이언트에 무엇이 필요한지가 다릅니다.

### Admin API 모드 (권장)

controller 의 admin API 를 켜면 클라이언트에 **DB·오브젝트 스토리지 자격증명이 전혀 필요 없습니다**. 빌드 산출물을 tar 로 스트리밍하면 나머지는 controller 가 처리합니다.

```sh
duru deploy --dir ./build-output --tenant acme --page blog \
  --admin-url http://controller:9450
```

admin API 는 **별도 포트**로 뜨고 `DURUPAGES_ADMIN_ENABLED=true` 로 활성화합니다. 배포되는 기본 바이너리는 **인증 없이** 서비스하므로 반드시 사설망에 두세요(Kubernetes 에서는 ClusterIP 포트로만 열리며, `kubectl port-forward svc/<release>-controller 9450:9450` 로 접근하거나 앞단에 인증 프록시를 두세요). 프로세스 내에서 인증을 강제하려면 아래 미들웨어를 사용하세요.

```sh
helm upgrade durupages deploy/chart/durupages --reuse-values \
  --set controller.adminApi.enabled=true
```

배포 외에 tenant/page 관리와 롤백도 지원합니다:

```
GET/POST        /v1/tenants          GET/DELETE /v1/tenants/{tenantId}
GET/POST        /v1/pages            GET/DELETE /v1/pages/{pageId}
PUT             /v1/pages/{pageId}/custom-domains
GET/POST        /v1/pages/{pageId}/deployments          # POST = tar(.gz) 업로드
POST            /v1/pages/{pageId}/deployments/{deploymentId}/activate   # 롤백
```

```sh
# 이전 deployment 로 롤백
curl -X POST http://controller:9450/v1/pages/blog/deployments/dep-123/activate
```

#### 인증 추가하기

인증 방식은 조직마다 달라 특정 정책을 내장하지 않고 확장점으로 열어 두었습니다. `net/http` 미들웨어를 `adminapi.New` 에 넘기고 자체 controller 바이너리를 조립하면 됩니다(Storage/PageProvider/Queue/Scaler 인터페이스와 동일한 패턴).

```go
auth := func(next http.Handler) http.Handler {
    return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
        if r.URL.Path == adminapi.HealthPath { // probe 는 자격증명을 제시할 수 없음
            next.ServeHTTP(w, r)
            return
        }
        if !valid(r.Header.Get("Authorization")) {
            http.Error(w, "unauthorized", http.StatusUnauthorized)
            return
        }
        next.ServeHTTP(w, r)
    })
}

h, err := adminapi.New(adminapi.Options{
    Provider: prov, Admin: prov, Storage: store,
    Middleware: []func(http.Handler) http.Handler{auth},
})
```

체인은 **바깥쪽부터** 실행되며 **모든 라우트가 감싸집니다** — 암묵적으로 면제되는 경로가 없으므로, 자격증명을 제시할 수 없는 health probe 는 위처럼 명시적으로 면제하세요. 요청 로깅은 체인 바깥에 있어 거부된 요청도 로그에 남습니다.

### Direct 모드

admin API 를 쓰지 않으면 CLI 가 Storage 와 PostgreSQL 에 직접 씁니다. 두 자격증명이 모두 필요합니다:

```sh
duru deploy --dir ./build-output --tenant acme --page blog \
  --pg-dsn postgres://... --s3-bucket durupages
```

## 배포

Kubernetes 배포는 Helm Chart 를 사용합니다.

```sh
# worker JWT 용 Ed25519 키쌍 생성 (controller 서명 / hub 검증)
openssl genpkey -algorithm ed25519 -out worker-jwt.key
openssl pkey -in worker-jwt.key -pubout -out worker-jwt.pub

helm install durupages deploy/chart/durupages \
  --set-file workerJwt.privateKeyPEM=worker-jwt.key \
  --set-file workerJwt.publicKeyPEM=worker-jwt.pub \
  --set postgres.dsn='postgres://...' \
  --set s3.bucket=durupages \
  --set router.pagesDomain=pages.example.com
```

Chart 는 controller/router/hub 와 worker 네임스페이스·ServiceAccount·RBAC·NetworkPolicy 를 설치합니다. worker Pod 는 controller 가 런타임에 직접 생성합니다. 자세한 값은 [deploy/chart/durupages/README.md](deploy/chart/durupages/README.md) 를 참고하세요.

컨테이너 이미지는 GHCR 에 발행됩니다:

```
ghcr.io/<owner>/durupages-controller:<version>
ghcr.io/<owner>/durupages-router:<version>
ghcr.io/<owner>/durupages-hub:<version>
```

## 확장 지점

기본 구현을 그대로 쓰거나, 인터페이스를 구현해 커스텀 바이너리로 조립할 수 있습니다.

| 인터페이스 | 기본 구현 | 용도 |
|---|---|---|
| `Storage` | S3 (MinIO 호환) | 정적 자산·worker 번들 저장 |
| `PageProvider` | PostgreSQL | 테넌트/page/배포의 원천, 라우팅 해석 |
| `Queue` | in-memory | 테넌트별 대기열 (예: Redis 로 교체 가능) |
| `Scaler` | target/max concurrency | worker Pod scale up/down 정책 |
| `Runtime` | workerd | worker 실행 엔진 |
| admin API `Middleware` | 없음 (인증 없음) | admin API 앞단의 인증·인가·감사 |

```go
ctrl, err := controller.New(controller.Options{
    Provider: myProvider,   // 커스텀 PageProvider
    Storage:  s3storage.New(...),
    Queue:    redisqueue.New(...),
    Scaler:   myScaler.New(...),
})
```

## 개발

```sh
go build ./...
go test -race ./...      # 또는 make test
make e2e                 # 통합 e2e (Docker 필요)
```

각 바이너리는 `--version` 으로 빌드 시 새겨진 버전을 출력합니다. 릴리스 빌드는 Go 크로스컴파일로 `linux/amd64`·`linux/arm64` 바이너리를 만들고, 이미지는 그 산출물을 패키징만 합니다.

## 프로젝트 상태

핵심 기능(정적 서빙, SSR worker, 멀티테넌시, lazy load, 무중단 배포, 오토스케일링, 사용량 계측)은 동작하며 e2e 로 검증됩니다. 다만 아직 초기 단계이며 아래 제약이 있습니다.

- **요청당 CPU 계측·제한 미구현** — 공식 workerd 는 요청 CPU 시간을 0 으로 보고합니다. isolate 별 **메모리(heap) 제한**은 `durupages-workerd` 에 구현·검증되어 있으나, CPU 계측/제한은 후속 작업입니다. 그때까지 `cpuTime` 은 0 으로 기록됩니다.
- **KV / D1 / R2 / Durable Objects 바인딩 미지원** (초기 버전 비목표).
- Preview 배포, 브랜치 alias 등 Cloudflare 플랫폼 부가 기능은 범위 밖입니다.

자세한 설계 근거와 트레이드오프는 [docs/ARCHITECTURE.md](docs/ARCHITECTURE.md) 에 정리되어 있습니다.

## 라이선스

[Eclipse Public License 2.0](LICENSE) — Copyright JC-Lab
