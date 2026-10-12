<!-- source: README.md; sha256: 008297f255e89d26769a8517dd1c425b15462564563cce18859b56b7640a570e -->
# Crabbox {#crabbox}

**에이전트를 위한 주문형 컴퓨터.**

## Crabbox란 {#what-crabbox-is}

Crabbox는 에이전트와 사람에게 테스트와 빌드용 컴퓨터를 제공합니다. 로컬 편집을 계속하는 동안 원격 머신이 커밋하지 않은 변경까지 포함한 작업 트리를 실행합니다. 출력을 스트리밍하고 종료 코드를 반환하며 공급자 계약에 따라 임대를 정리합니다.

공급자를 설정한 뒤 저장소의 기존 테스트 명령을 실행하세요.

```sh
crabbox run -- pnpm test
```

## 하나의 주기, 다양한 머신 {#one-loop-many-kinds-of-box}

표준 원격 실행 과정입니다.

1. 기존 작업 사본을 **로컬에서 편집**합니다.
2. **머신을 임대**하거나 준비된 자원을 재사용합니다.
3. 커밋하지 않은 변경까지 작업 트리를 **동기화**합니다.
4. 머신에서 명령을 **실행**합니다.
5. 호출자에게 **출력과 종료 코드를 전달**합니다.
6. 소유 자원을 **정리**하거나 다음 실행을 위해 유지합니다.

위임형 공급자는 실행과 동기화 경로를 맡을 수 있습니다. 재사용, 증거, 정리는 공급자 기능과 실행 정책을 따릅니다.

기반 인프라를 바꾸어도 공급자 전용 스크립트로 작업 흐름을 바꿀 필요가 없습니다. 작업만 정해졌다면 [활용 사례](use-cases.md), 실행 위치를 안다면 [공급자 참조](providers/README.md)를 보세요.

## 더 많은 에이전트, 더 적은 대기 {#more-agents-less-waiting}

- **병렬 에이전트:** 테스트와 빌드를 별도 머신에서 실행하여 노트북의 CPU, RAM, 포트 경쟁을 피합니다.
- **준비된 머신:** 환경을 재사용하여 편집·실행 주기를 줄입니다.
- **교차 플랫폼:** 같은 작업 사본에서 Linux, macOS, 네이티브 Windows, WSL2를 대상으로 합니다. 지원 여부는 공급자에 따라 다릅니다.
- **보고 제어하기:** 지원 데스크톱에서 [VNC 또는 WebVNC](features/interactive-desktop-vnc.md), 브라우저, 관리형 Linux의 `--code`를 통한 브라우저 VS Code를 사용합니다. 인증된 [포털](features/portal.md)은 임대와 로그를 보여 줍니다. 로컬 브리지가 실행 중이면 WebVNC로 관찰하거나 제어할 수 있습니다.
- **팀 자원 공유:** 선택적 조정자가 클라우드 자격 증명을 보관하고 임대를 공유하며 중개 공급자의 만료와 지출 한도를 적용합니다.

[공급자 카탈로그](providers/README.md)는 클라우드, 로컬 VM, SSH 호스트, 위임형 샌드박스를 다룹니다. [외부 플러그인 계약](providers/external.md)으로 자체 백엔드를 연결합니다. 사이트의 공급자 목록과 분류별 수는 카탈로그 메타데이터에서 생성됩니다.

## 시작하기 {#start-here}

- [시작 가이드](getting-started.md) — CLI 설치와 첫 원격 실행.
- [활용 사례](use-cases.md) — 빠른 피드백, 에이전트 실행, 플랫폼 검증, 브라우저 QA, 병렬 확장, GPU.
- [가격과 비용](pricing.md) — 오픈 소스 및 자체 컴퓨팅 자원 비용 모델과 보호 장치.
- [Crabbox 작동 방식](how-it-works.md) — CLI, 조정자, 실행기를 거치는 실행.
- [Crabbox 비전](../VISION.md) — 제품 범위, 비목표, 수명 주기 안전 규칙.

## 운영 모델 선택 {#pick-an-operating-model}

| 경로 | 적합한 용도 | 소유권 |
| --- | --- | --- |
| 로컬 런타임 | 자격 증명 없는 빠른 개발 검사 | 사용자 워크스테이션과 런타임 |
| 직접 클라우드 또는 SSH | 개인 계정, 사설 호스트, 자체 가상화 | 사용자 계정 또는 인프라 |
| 팀 조정자 | 자격 증명, 임대, 정리, 사용량, 지출 한도 공유 | 사용자 Cloudflare 또는 Node.js/PostgreSQL 배포 |
| 위임 실행 | 공급자 주도 에이전트, CI, 브라우저, GPU | 선택한 공급자 또는 운영자 |

Crabbox는 MIT 라이선스입니다. 현재 호스팅 제어 평면 요금제는 없으며 컴퓨팅과 조정자 인프라는 각 운영자가 과금합니다. 정확한 경계는 [가격과 비용](pricing.md)을 보세요.

## 신뢰 경계 {#trust-boundary}

신뢰할 수 있는 동료와 저장소를 실행하세요. 격리는 런타임에 따라 다릅니다. 재사용과 파괴적 정리는 정확한 자원과 claim의 소유권을 확인해야 하며 불분명하면 거부합니다.

[공급자 선택](features/provider-selection.md)으로 작업을 라우팅한 뒤 공급자 문서와 [보안 모델](security.md)을 읽으세요. 추천 명령은 작업 지침이며 보안 인증이 아닙니다.

## 더 알아보기 {#go-deeper}

- **CLI와 설정:** [CLI](cli.md), [명령 참조](commands/README.md), [설정](features/configuration.md), [저장소 온보딩](features/repository-onboarding.md).
- **머신 풀과 운영:** [아키텍처](architecture.md), [인프라](infrastructure.md), [운영](operations.md), [관측성](observability.md), [읽기 전용 장치 페어링](features/device-pairing.md), [보안](security.md).
- **실행과 증거:** [작업](features/jobs.md), [Actions 환경 준비](features/actions-hydration.md), [아티팩트](features/artifacts.md), [체크포인트](features/checkpoints.md), [대화형 데스크톱과 VNC](features/interactive-desktop-vnc.md).
- **플랫폼 경계:** [중첩 실행](features/nested-execution.md)은 WSL2, 컨테이너 엔진, 준비된 KVM 호스트, 로컬 샌드박스를 구분합니다.
- **확장:** [통합 카탈로그](integrations/README.md), [공급자 작성](features/provider-authoring.md), [소스 맵](source-map.md).

## 문서 안내 {#about-these-docs}

`docs/`의 Markdown이 [crabbox.sh](https://crabbox.sh/)의 사용자 문서 원본입니다. 구현의 기준은 코드이며 [소스 맵](source-map.md)이 담당 구현을 연결합니다.

로컬에서 사이트를 빌드하고 검증하세요.

```sh
scripts/check-docs.sh
open dist/docs-site/index.html
```

`docs/`에서는 지원되는 1–4단계 제목과 백틱 세 개 코드 펜스를 사용합니다. 제목 링크는 렌더러와 같은 ID로 검사하며 예제와 주석은 앵커를 예약하지 않습니다. 공개 제목 ID를 유지하세요. 저장소 전용 Markdown은 기존 GitHub 방식 앵커 규칙을 사용하며 GitHub Markdown 전체 지원을 뜻하지 않습니다.

언어 추가·갱신은 [문서 번역 가이드](translations.md)를 보세요。
