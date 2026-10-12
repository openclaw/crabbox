<!-- source: use-cases.md; sha256: 769c5e58a1b095392245d30f0377bf87b025fa28c3eeca64f9337340d43cd3d7 -->
# 활용 사례 {#use-cases}

각 코딩 에이전트에 테스트와 빌드용 주문형 컴퓨터를 제공합니다. 에이전트가 로컬 작업 사본을 계속 편집하는 동안 Crabbox는 커밋하지 않은 변경 사항을 동기화하고 명령을 원격 실행하여 출력과 종료 코드를 반환합니다. 별도의 원격 임대는 병렬 작업이 노트북의 CPU, 메모리, 포트를 두고 경쟁하지 않게 합니다.

플랫폼 동작이 중요하면 Linux, macOS, 네이티브 Windows 또는 WSL2를 선택하세요. 반복 실행에는 준비된 머신을 재사용하고, 결과를 살펴볼 때는 데스크톱이나 브라우저 편집기를 여세요. 대화형 접근과 인계 방법은 [데스크톱, VNC, 브라우저, Code 및 포털 가이드](features/interactive-desktop-vnc.md)를 참고하세요.

공급업체보다 워크로드에서 출발하세요.

```sh
crabbox providers recommend
crabbox providers recommend fast-feedback
crabbox providers recommend agent-sandbox --json
```

용도를 생략하면 `crabbox providers recommend`가 지원하는 추천 이름을 모두 표시합니다. 추천은 저장소의 기능 메타데이터를 사용합니다. 공급자 접속, 자격 증명이나 할당량 확인, 시작 시간 측정, 보안 경계 인증을 수행하지 않습니다.

## 테스트와 빌드 주기 단축 {#speed-up-test-and-build-loops}

저장소 명령을 로컬에서 실행하기에 너무 느리거나 메모리를 많이 쓰거나 다른 작업을 방해할 때 사용하세요.

```sh
crabbox providers recommend fast-feedback
crabbox providers recommend linux-vm
crabbox run --provider hetzner -- pnpm test
```

로컬 런타임, 재사용 캐시, 빠른 편집·실행 주기에는 `fast-feedback`을 선택하세요. 작업을 워크스테이션 밖으로 옮기거나 완전한 VM 및 SSH 동작이 필요하면 `linux-vm`을 선택하세요.

## 코딩 에이전트에 일회용 환경 제공 {#give-a-coding-agent-a-disposable-environment}

에이전트가 워크스테이션 밖에서 저장소 명령을 실행해야 하면 원격 SSH 임대나 위임형 샌드박스를 사용하세요. 로컬 샌드박스는 작업 공간을 분리하지만 노트북 자원은 계속 사용합니다. Crabbox는 실행과 정리를, 호출하는 에이전트나 하네스는 오케스트레이션을 담당합니다.

독립적인 에이전트마다 각 작업 사본에서 별도 임대를 만드세요.

```sh
crabbox warmup --provider hetzner --class standard --slug agent-tests
crabbox run --id agent-tests -- pnpm test
crabbox stop agent-tests
```

임대마다 고유한 slug를 사용하세요. 준비된 임대는 명령 사이에 설정 상태를 보존합니다. 테스트 전에 저장소 설정을 검토하고 도구와 의존성을 설치하세요.

```sh
crabbox providers recommend agent-sandbox
crabbox providers recommend disposable-execution
crabbox providers recommend isolated-execution
```

이 세 가지 추천은 서로 다른 요구에 답합니다.

- `agent-sandbox`는 에이전트 및 개발 환경 실행에 맞는 공급자를 우선합니다.
- `disposable-execution`은 정리 가능한 임시 런타임을 요구합니다.
- `isolated-execution`은 위임형 및 로컬 샌드박스 경계를 우선합니다.

마지막 추천은 라우팅 지침이지 보안 인증이 아닙니다. Crabbox는 신뢰할 수 있는 운영자, 저장소 설정, 워크로드를 전제로 합니다. 일회용 머신이 악성 코드를 안전하게 만들지는 않습니다. 비밀 정보를 전달하기 전에 공급자의 신뢰 및 네트워크 모델을 확인하세요.

## Linux, Windows, WSL2 또는 macOS 검증 {#validate-linux-windows-wsl2-or-macos}

대상 운영 체제에 따라 동작이 달라지면 플랫폼 추천을 사용하세요.

```sh
crabbox providers recommend linux-vm
crabbox providers recommend windows
crabbox providers recommend macos

crabbox run --provider aws --target windows --windows-mode wsl2 -- uname -a
```

네이티브 Windows는 PowerShell과 Windows 도구를 실행합니다. WSL2는 중첩 가상화를 지원하는 Windows VM 안에서 POSIX 계약을 실행합니다. macOS 대상은 공급자에 따라 로컬 Apple Silicon VM 또는 클라우드 Mac입니다.

## 브라우저 또는 시각적 데스크톱 테스트 {#test-a-browser-or-visible-desktop}

브라우저, 스크린샷, 입력 또는 팀원에게 인계할 세션이 필요하면 데스크톱 지원 SSH 임대를 사용하세요.

```sh
crabbox providers recommend desktop
crabbox warmup --provider azure --target windows --windows-mode normal --desktop --slug ui-check
crabbox webvnc --id ui-check --open --take-control
```

WebVNC는 권한이 있는 팀원이 데스크톱을 보고 제어하게 합니다. 네이티브 VNC는 SSH 터널을 사용하며 `crabbox code`는 지원되는 Linux 임대에서 code-server 편집기를 엽니다. 조정자 포털에는 임대, 브리지 상태, 보존된 실행 증거가 표시됩니다. 브라우저를 사용하는 동안 로컬 브리지를 유지하세요. 사전 조건은 [통합 접근 가이드](features/interactive-desktop-vnc.md)를 참고하세요.

머신을 해제한 뒤에도 결과가 필요하면 증거 라우팅을 함께 선택하세요.

```sh
crabbox providers recommend run-evidence
crabbox providers recommend failure-diagnostics
```

## 준비된 상태 재사용 {#reuse-warm-state}

반복 설정 비용이 준비된 상태를 유지하거나 복원하는 비용보다 클 때 사용하세요.

```sh
crabbox providers recommend warm-start
crabbox providers recommend versioned-workspace

crabbox warmup --slug warm-tests
crabbox run --id warm-tests -- pnpm test:changed
```

`warm-start`는 로컬 런타임, 캐시 볼륨, 유지된 세션, 일시 중지/재개, 작업 공간 상태 등의 신호를 사용합니다. 모든 공급자에서 같은 스냅샷이나 웜 풀 기능을 보장하지 않습니다.

## 병렬 실험 확장 {#fan-out-parallel-experiments}

병렬 브랜치, 에이전트의 여러 후보 시도, 준비된 상태에서 시작할 테스트 샤드에는 fork 지원 작업 공간을 사용하세요.

```sh
crabbox providers recommend fanout-testing
crabbox checkpoint fork <checkpoint-id> --count 4
```

공급자 중립적인 체크포인트 및 fork 라우팅이며, microVM의 실시간 메모리 fork를 보장하지 않습니다.

## GPU 워크로드 실행 {#run-gpu-workloads}

모델 테스트, CUDA 빌드, 렌더링 등 가속기 작업에는 GPU 추천을 사용하세요.

```sh
crabbox providers recommend gpu
crabbox run --provider runpod -- nvidia-smi
```

대화형 디버깅에는 SSH 임대 GPU 공급자가 대체로 적합합니다. 공급자가 실행과 결과 수집을 담당해야 하면 위임형 GPU가 더 적합한 경우가 많습니다.

## 클라우드 자격 증명 없이 로컬 실행 {#run-locally-without-cloud-credentials}

로컬 컨테이너, VM 또는 정책 샌드박스를 사용하여 워크스테이션 안에서 빠른 스모크 테스트를 수행하세요.

```sh
crabbox providers recommend local
crabbox providers recommend offline-validation
crabbox run --provider local-container -- pnpm test
```

로컬이라고 격리 모델이 같지는 않습니다. Docker 호환 컨테이너, Apple VM, Multipass VM, Windows Sandbox, 로컬 정책 런타임은 호스트 접근, 지속성, 정리 계약이 다릅니다. 낯선 코드를 실행하기 전에 공급자 문서를 읽으세요.

## 기존 인프라 사용 {#use-infrastructure-you-already-own}

공유 조정자 없이 상시 SSH 호스트, 사설 가상화 스택 또는 직접 클라우드 계정을 사용할 수 있습니다.

```sh
crabbox providers recommend byo-ssh
crabbox providers recommend self-hosted
crabbox providers recommend linux-vm
```

Crabbox가 수명 주기를 소유하면 안 되는 호스트에는 `ssh`를 사용하세요. 런타임 생성과 정리를 맡기려면 Proxmox, XCP-ng, Incus, KubeVirt, Firecracker 같은 자체 호스팅 공급자를 사용하세요.

## 관리되는 팀 머신 풀 공유 {#share-a-governed-team-fleet}

공급자 자격 증명, 임대 소유권, 정리, 사용 기록, 활성 임대 제한, 월간 지출 한도를 공유할 때 조정자 경로를 사용하세요.

```sh
crabbox providers recommend team-cloud
crabbox login --url https://broker.example.com
crabbox usage --scope user
```

조정자는 제어 평면입니다. CLI는 일반 SSH 실행기에 직접 연결하여 동기화하고 실행하며 조정자에 증거를 기록합니다. `/portal`에서 임대 목록과 데스크톱, 편집기, 실행 상세 링크를 볼 수 있습니다. 직접 공급자 등록은 자격 증명이나 정리 소유권을 넘기지 않고 포털 가시성을 추가합니다.

## 더 구체적인 경로가 필요한가요? {#need-a-more-specific-route}

CLI는 아티팩트, CI 증명, 코드 인터프리터, 장애 진단, MCP 연결, 네트워크 제한, 일시 중지/재개, 미리 보기 URL, 연결 가능성, 원격 개발, 자원 관측, 재사용 실행 세션, 웹 앱 스모크 테스트, Worker/모듈 실행도 추천합니다.

최신 전체 목록, 별칭, 필터, 점수, 주의 사항은 [`crabbox providers recommend`](commands/providers.md#providers-recommend)를 참고하세요.

## 관련 문서 {#related-docs}

- [공급자 선택](features/provider-selection.md)
- [공급자 참조](providers/README.md)
- [중첩 실행](features/nested-execution.md)
- [가격과 비용](pricing.md)
