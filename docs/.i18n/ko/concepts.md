<!-- source: concepts.md; sha256: 27d2835bb5b9d8ffdb9a39d681b5f2c01215e10c33a0199b1b1e2dd3347e29c8 -->
# 개념 {#concepts}

Crabbox는 에이전트와 사람에게 컴퓨터를 제공합니다. 로컬 편집 후 **임대**로 머신을 확보하고 **실행**으로 동기화와 명령을 수행합니다. 별도 머신은 CPU, 메모리, 포트 경쟁 없이 병렬 테스트를 지원합니다.

Linux, macOS, 네이티브 Windows, WSL2를 지원합니다. 준비된 임대는 환경을 유지하고 Desktop, Browser, Code와 포털은 접근과 증거를 제공합니다. [접근 가이드](features/interactive-desktop-vnc.md)를 보세요.

CLI, 조정자, 문서의 공통 용어입니다. 권장 동의어는 **굵게** 표시합니다.

## 컴퓨팅 용어 {#compute-vocabulary}

**Lease:** 생성하거나 확인한 실행기의 기한부 예약. `cbx_...`, slug, 유휴 제한, TTL, `active`, `released`, `expired`, `failed` 상태를 가지며 비용과 정리의 단위입니다.

**Runner:** 공급자가 준비한 실행 머신. Hetzner, AWS EC2, 정적 SSH 모두 같은 실행기 개념입니다.

**Box** / **testbox:** 비공식 동의어. 새 문서에는 runner를 권장하며 준비된 머신 같은 제품 표현에는 box를 씁니다.

**Pool:** 사용자, 조직, 전체 풀에 보이는 활성 실행기. `crabbox list`와 관리용 `GET /v1/pool`이 표시합니다.

**Pond:** `pond=<name>` 레이블을 공유하는 자연 발생 그룹. 중앙 객체 없이 활성 구성원이 있을 때만 존재합니다. `crabbox pond peers`, `crabbox pond
connect`/`disconnect`, `crabbox pond release`를 사용합니다. [Pond](features/pond.md).

**Lease ID:** `cbx_`와 소문자 16진수 12자리, 총 16자. 예: `cbx_abcdef123456`. 레이블, 로그, claim, API에 사용합니다. [식별자](features/identifiers.md).

**Slug:** 단어 쌍과 8자리 지문의 친숙한 이름. 예: `swift-crab-1f3a9c2b`. 충돌 시 4자리 추가. `--slug swift-crab`로 사용자 지정하며 `--slug`는 `[a-z0-9-]` 최대 41자로 정규화합니다. `--id`에 이름과 ID 모두 사용합니다.

**Run:** `crabbox run` 호출 한 번. `run_` ID, 임대, 명령, 종료 코드. 조정자에는 영속 이력, 직접 실행에는 로컬 시간·증거·실패 아티팩트가 있습니다. [이력과 로그](features/history-logs.md).

**Workspace:** 작업 사본과 상태, 파일, 의존성, 공급자 작업 디렉터리, 출력, 선택적 화면 상태. 머신 이상의 사용자 환경을 뜻합니다.

## 역할 {#roles}

**CLI:** 로컬 Go 바이너리 `crabbox`, 설정, 동기화, 실행, 출력, SSH 키 담당. [아키텍처](architecture.md).

**Broker** / **coordinator:** 인증된 공유 제어 평면. 자격 증명, 상태, 만료, 정리, 이력, 사용량, 한도. Cloudflare Workers/Fleet Durable Object 또는 Node.js/PostgreSQL/pg-boss. 서비스 강조는 coordinator, 신뢰 경계 강조는 broker입니다.

**Provider:** 확보, 확인, 목록, 해제 어댑터. AWS, Azure, Google Cloud, Hetzner; Proxmox, Parallels, Local Container, SSH; SSH 샌드박스 Daytona, KubeVirt, External, Namespace Devbox, Semaphore, Sprites, exe.dev, RunPod, Railway; 위임형 Blacksmith Testbox, E2B, Modal, Islo, Tensorlake, [Upstash Box](https://upstash.com/docs/box/overall/quickstart), Cloudflare, Azure Dynamic Sessions, W&B. [참조](providers/README.md).

**Backend:** 실제 SSH 대상의 `SSHLeaseBackend`와 공급자가 실행하는 `DelegatedRunBackend` Go 인터페이스. [백엔드](provider-backends.md).

**Operator:** 관리 권한이 있는 사람. `crabbox admin` 및 이미지 생성·승격을 실행합니다.

**Agent:** CLI를 호출하는 LLM 프로세스. Crabbox는 실행·증거를, 에이전트/하네스는 프롬프트·판단·오케스트레이션을 소유합니다. 설정과 작업은 신뢰되어야 합니다.

## 모드 {#modes}

**Brokered mode** / **coordinator mode:** 조정자가 생성·상태·정리, CLI가 직접 SSH/rsync/실행을 담당합니다. 비밀은 중앙에 둡니다. AWS, Azure, Daytona, Google Cloud, Hetzner와 `CRABBOX_COORDINATOR` 또는 `config set-broker`를 사용합니다.

**Direct mode** / **direct-provider mode:** API 직접 접근, 중앙 이력과 지출 한도 없음. 다섯 공급자 외에는 유일한 방식이며 이들도 URL 없으면 사용합니다.

**Static mode:** `provider: ssh`, 운영자 소유 호스트, 생성·삭제 없음, 항상 직접입니다.

**Delegated mode:** Blacksmith, E2B, Modal, Islo 등이 실행하고 출력을 반환합니다. `--sync-only`, `--checksum`은 거부되며 `sync=delegated`로 표시됩니다.

## 명령 {#commands}

**warmup:** 임대 확보·준비, `--keep` 기본 true, 아직 명령은 실행하지 않습니다.

**run:** 확보/재사용, 동기화, 실행, 출력과 코드. 새 임대는 해제하고 재사용은 유지합니다. `--keep`, `--stop-after`로 제어합니다.

**stop** / **release:** 공급자 계약에 따라 종료합니다. 보통 일회용 자원 삭제, 정적·보존 자원은 예외입니다.

**cleanup:** 정확한 소유권 검증 후 적합한 직접 자원 정리. 이름, ID, 레이블만으로는 삭제할 수 없습니다.

**reuse:** `--id`/slug로 `run`의 기존 임대를 선택합니다. `warmup`은 생성하며 `--lease-id`는 멱등 생성이지 일반 재사용이 아닙니다.

**reclaim:** 기본 저장소 바인딩을 넘겨 다른 작업 사본에서 쓰도록 claim 이동(`--reclaim`).

**hydrate:** 저장소 Actions로 의존성을 준비합니다. 지원 설정 단계를 실행하며 완전한 의미가 필요하면 `--github-runner`로 등록하고 실제 작업을 실행합니다. [Actions 준비](features/actions-hydration.md).

## 상태 {#state}

**Idle timeout:** 하트비트 없이 해제까지 시간, 기본 1800초/30분. 하트비트나 touch로 재설정됩니다.

**TTL:** 절대 수명 5400초/90분, 최대 86400초/24시간. 하트비트로 연장 불가. `expiresAt = min(createdAt + ttl, lastTouchedAt + idleTimeout)`.

**Heartbeat:** `POST /v1/leases/{id}/heartbeat`가 `lastTouchedAt`을 갱신하며 선택적 원격 측정과 명시적 유휴 변경을 보냅니다. `expiresAt`부터 거부하고 정리가 소유합니다.

**Touch:** 상태/유휴 갱신. 직접 공급자는 `Touch`, 중개 대응은 하트비트입니다.

**Reserved cost:** `hourlyRate × ttl` 최악 비용을 월간 한도에서 예약하고 해제 시 반환합니다.

**Estimated cost:** `active` 시간과 시간당 가격의 추정치, `crabbox usage`에 표시합니다. [비용과 사용량](features/cost-usage.md).

## 동기화 {#sync}

**Manifest:** `git ls-files --cached`와 `git ls-files --others --exclude-standard`에서 만든 NUL 구분 경로. 삭제는 별도입니다.

**Fingerprint:** 커밋, 변경 메타데이터, manifest 해시. 일치하면 rsync를 생략합니다.

**Git seeding:** origin/base에서 원격 Git을 준비하여 차이 비교를 가능하게 하는 선택적 첫 동기화 단계.

**Base ref:** 준비·hydration 기준 Git 참조, `sync.baseRef`로 설정합니다.

**Sanity check:** rsync 후 대량 삭제, manifest 누락, 의심 결과를 검사합니다. [동기화](features/sync.md).

## 기능 {#capabilities}

**Desktop:** `--desktop`으로 관리 Linux/새 컨테이너의 크기 조절 TigerVNC/XFCE 또는 Wayland/GNOME을 추가합니다. `crabbox
vnc`, `crabbox webvnc`, 대부분의 화면형 `--browser`에 필요합니다.

**Browser:** `--browser`가 브라우저를 설치/발견하고 경로를 제공합니다. 헤드리스 가능, `--desktop`으로 화면 표시. 엔진은 공급자/이미지에 달려 있으며 로그인 프로필은 없습니다.

**Code:** `--code`가 loopback 8080의 code-server를 설치합니다. `crabbox code`, `/code/`에 사용하며 관리 Linux 전용입니다.

**Tailscale:** 공개 IP 없이 tailnet으로 관리 Linux 접근. 평면 선택 `--network tailscale`과 별개입니다. [기능](features/capabilities.md).

## 조정자 런타임 {#coordinator-runtime}

**FleetCoordinator:** 공통 TypeScript 동작. 라우트, 인증, 어댑터, 기록, 비용, 정리, 포털, 브리지.

**Durable Object:** Cloudflare 상태와 직렬화 결정. `idFromName("default")`의 Fleet 하나를 사용합니다.

**PostgreSQL runtime:** 이식형 Node, `crabbox` 스키마 키/값과 `crabbox_jobs`의 pg-boss 정확한 유지 관리 및 정기 조정.

**Alarm:** 미래 정리 기한. Durable Object/pg-boss가 유휴, TTL, 재시도, 유지 관리를 예약합니다.

**Portal:** `/portal/...`의 서버 렌더링 인증 UI. 임대, 로그, 이벤트, VNC/Code. [포털](features/portal.md).

**Bridge:** 로컬 CLI 릴레이 연결. WebVNC/Code는 loopback VNC 5900/code-server 8080 SSH 터널을 유지하며 브라우저 사용 중 계속 실행해야 합니다. Egress는 별도 티켓 릴레이. 조정자는 인증하며 직접 SSH를 열지 않습니다.

## 신원 {#identity}

**Owner:** 임대 소유자. GitHub는 불변 `github:<numeric-id>`로 고정합니다. 공유 자동화는 검증된 설정 소유자나 Git 이메일을 사용합니다. [운영 보안](security.md#authentication).

**Org:** 토큰이나 `CRABBOX_ORG`의 조직, 사용량 범위와 다중 테넌트 한도에 사용합니다.

**Allowed org:** 서명 로그인 토큰 발급에 필요한 조직 멤버십, Worker별 설정.

**Admin token:** `GET /v1/pool`, 관리 라우트, 전체 목록용 별도 권한. 공유 토큰보다 엄격히 보관합니다.

**Cloudflare Access:** 선택적 앞단 보호. 검증된 `cf-access-jwt-assertion`만 신뢰합니다.

## 저장 {#storage}

**State directory:** claim, 이력, 체크포인트. `XDG_STATE_HOME`은 `$XDG_STATE_HOME/crabbox`와 키/호스트 신뢰까지 선택합니다. 없으면 `<os-user-config-dir>/crabbox/state`, 키는 `<os-user-config-dir>/crabbox/testboxes`. 루트 변경은 옛 상태를 이전하거나 검색하지 않습니다. [SSH 키](features/ssh-keys.md).

**Claim:** 임대와 작업 사본을 묶는 JSON. `crabbox run --id`의 slug 확인과 `--reclaim` 없는 다른 저장소 재사용 거부에 필요합니다.

**Workdir** / **work root:** 동기화 대상. Linux `/work/crabbox`, 일반 Windows `C:\crabbox`, 그 외 공급자·대상별입니다.

## 증거와 아티팩트 {#evidence-and-artifacts}

**Run history:** `crabbox history`, `logs`, `events`, `results`의 과거 기록, 현재 임대 상태와 다릅니다. [이력](features/history-logs.md).

**Test results:** `crabbox results <run-id>`의 JUnit 스위트, 테스트, 실패, 오류, 건너뜀. [결과](features/test-results.md).

**Telemetry:** 실행 전후 Linux 부하, 메모리, 디스크, 가동 시간 샘플. 제품 분석이 아닙니다. [원격 측정](features/telemetry.md).

**Artifacts:** `crabbox artifacts`의 화면, 비디오, GIF, 로그, 상태, 메타데이터. PR에 게시할 수 있습니다. [아티팩트](features/artifacts.md).

**Egress:** `crabbox egress`가 WebSocket으로 운영자 머신을 통해 아웃바운드 트래픽을 전달합니다. [Egress](features/egress.md).

**Checkpoint:** 복원/fork 가능한 VM·작업 공간 상태, `chk_...`, 아카이브, 레시피, 원시 스냅샷/이미지. [체크포인트](features/checkpoints.md).

**Cache volume:** 저장소/런타임/플랫폼별 절대 경로의 영속 캐시, 속도 전용. fork 가능한 시나리오가 아니고 작업 트리가 기준입니다. [캐시](features/cache.md).

**Capsule:** `crabbox capsule from-actions`로 수집하고 `crabbox capsule
replay`로 재현하는 이식형 실패 묶음. [캡슐](features/capsules.md).

## 문서 {#documentation}

**Source map:** 동작을 구현과 연결합니다. [소스 맵](source-map.md).

**Feature page:** `docs/features/<name>.md`, 명령과 공급자가 참조하는 개념 설명.

**Command page:** `docs/commands/<name>.md`, `--help`와 맞는 플래그, 동작, 종료 코드.

**Provider page:** `docs/providers/<name>.md`, 대상, 설정, 환경 변수, 동기화, 실패.

관련 문서:

- [작동 방식](how-it-works.md)
- [아키텍처](architecture.md)
- [CLI](cli.md)
- [설정](features/configuration.md)
- [백엔드](provider-backends.md)
