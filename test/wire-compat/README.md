# wire-compat — kube-loxilb ↔ loxilb 페이로드 회귀 게이트

두 kube-loxilb 빌드에 **같은 Kubernetes Service 집합**을 물린 뒤, 각자가 loxilb REST API 로
내보내는 **요청 바디를 바이트 단위로 비교**한다. 클러스터와 가짜 loxilb 만 있으면 되므로
VM·BGP·실제 loxilb 없이 몇 분 안에 돌아간다.

이 하네스가 답하는 질문은 하나다:

> 이 변경이 **기존 Service 에 대해** loxilb 로 나가는 요청을 바꾸는가?

loxilb-inference-gateway 확장처럼 payload 모델에 필드를 추가하는 작업에서, 신규 필드가
plain loxilb 로 새어 나가지 않는지 확인하는 것이 주 용도다.

## 무엇을 잡아내나

실제로 이 하네스가 잡아낸 것들:

- **payload 오염** — 게이트웨이 전용 필드(`ep_role`, `nixl_port`, `url_path`, `pd_*`, `kv_*` …)가
  plain loxilb 로 전송되는지. JSON 키 집합을 통째로 비교한다.
- **열거형 값 변화** — `loxilb.io/epselect` 같은 어노테이션이 전과 다른 `sel` 값을 보내는지.
- **거부 응답 처리** — 409(이미 있는 룰 = 정상 상태)와 404(룰이 사라짐 = 이상 상태)를
  구분하는지. 문자열 매칭으로 판정하던 구현은 `not-exists` 안의 `exist` 에 걸려
  **404 를 성공으로 읽는다**. 그 상태에서도 Service 에는 ExternalIP 가 붙어 정상처럼 보이므로,
  이 하네스 없이는 무증상으로 지나간다.
- **엔드포인트 변화 대응** — pod 증감 시 재프로그래밍 횟수와 시점.
- **버전 조회 실패** — `/version` 이 404 인 구형 피어에서도 룰이 프로그래밍되는지.

## 준비물

`k3d`, `kubectl`, `docker`, `python3`. 클러스터 노드 이미지와 엔드포인트 이미지는
스크립트가 알아서 받는다.

## 실행

```bash
# 비교 기준 바이너리 빌드 (예: main)
git worktree add /tmp/wt-main main
(cd /tmp/wt-main && go build -o /tmp/kube-loxilb-main ./cmd/loxilb-agent)

# 후보 바이너리 빌드 (현재 브랜치)
go build -o /tmp/kube-loxilb-cand ./cmd/loxilb-agent

# 전체 A/B
./run_ab.sh /tmp/kube-loxilb-main /tmp/kube-loxilb-cand

# 정리
./teardown.sh
```

산출물은 전부 `_out/` 에 남는다 (`rec-*.jsonl` 요청 기록, `log-*.txt` 에이전트 로그,
`svc-*.json` Service 상태). git 에 올라가지 않는다.

개별 실행도 된다:

```bash
./setup.sh
./run_case.sh /tmp/kube-loxilb-cand mycase normal plain 70
./run_case.sh /tmp/kube-loxilb-cand rejected notfound plain 45
python3 compare.py _out/rec-a.jsonl _out/rec-b.jsonl a b
```

## 읽는 법

`compare.py` 는 각 룰을 `(port, protocol)` 로 키잡아 정렬하고, **풀에서 할당되는 externalIP 는
마스킹**한 뒤(할당 순서에 따라 달라지므로) 필드 단위로 비교한다. 엔드포인트 순서도 정규화한다.

```
main: 19 rules   igw: 19 rules
  DIFF port=8015/udp   serviceArguments.sel: main=7  igw=6
2 RULE(S) DIFFER (19 rules compared)
```

**합격 기준은 `IDENTICAL`** 이다. diff 가 나오면 그 하나하나가 의도한 변경인지 설명할 수 있어야 한다.
`summarize.py` 표에서는 거부 모드별로 요청 수 / ExternalIP 부여 수 / 에러 로그 수를 비교한다 —
`notfound` 모드에서 요청 수가 적고 ExternalIP 가 전부 붙었다면 **404 를 성공으로 오독**하고 있는 것이다.

### 이미 알려진 정상 diff

`integration/inference-gateway` 를 `main` 과 비교하면 아래 2건이 나온다. 둘 다 의도된 정정이다.

| 픽스처 | diff | 설명 |
|---|---|---|
| `f4-n3` (8015/udp) | `sel: 7 → 6` | upstream loxilb `common/common.go` 의 실제 enum 은 `n3=6`. main 은 존재하지 않는 `n2det` 를 끼워 넣어 7 을 보내고 있었다 |
| `f5-n2det` (8016/udp) | `sel: 6 → 0` | `n2det` 값 제거. 알 수 없는 값이므로 rr 로 폴백하고 경고 로그를 남긴다 |

## 픽스처 (`fixtures.yaml`)

Service 하나당 **포트 하나**를 배정해 두 실행 사이에서 룰을 짝지을 수 있게 했고,
`nodePort` 도 고정했다 — 고정하지 않으면 재생성 때마다 값이 바뀌어 모든 룰이 diff 로 뜬다.

| 픽스처 | 포트 | 검증 대상 |
|---|---|---|
| `f1-plain` | 8001 | 어노테이션 없는 기본 TCP |
| `f2-udp` / `f2-sctp` | 8002 / 8003 | 프로토콜별 |
| `f3-*` | 8004~8007 | `lbmode`: fullnat / dsr / onearm / fullproxy |
| `f4-*` | 8010~8015 | `epselect`: rr / hash / persist / lc / n2 / n3 |
| `f5-n2det` | 8016 | 제거된 `epselect` 값의 폴백 |
| `f6-probe` | 8020 | probe 계열 어노테이션 6종 |
| `f7-misc` | 8021 | liveness / timeout / prefLocalPod / useproxyprotov2 |
| `f8-static` | 8022 | staticIP + poolSelect |
| `f9-mtls` | 8023 | mTLS frontend (Secret `mtls-front` 사용) |
| `f10-scale` | 8030 | 예비 |
| `f11-podnet` | 8031 | `usepodnetwork` — 엔드포인트 증감 시나리오용 |

새 어노테이션을 추가하면 **여기에 픽스처를 한 줄 늘리는 것이 이 하네스를 유지하는 방법**이다.

## 가짜 loxilb (`fake_loxilb.py`)

`POST`/`DELETE` 바디를 jsonl 로 기록하고, 환경변수로 응답을 바꾼다.

| 변수 | 값 | 의미 |
|---|---|---|
| `FAKE_MODE` | `normal` | `200 {"result":"Success"}` |
| | `conflict` | `409 {"result":"lb rule exists"}` — 이미 있는 룰 |
| | `notfound` | `404 {"result":"not-exists"}` — 사라진 룰 |
| `FAKE_VERSION` | `plain` | `product` 없는 `/version` = upstream loxilb |
| | `missing` | `/version` 404 = 구형 피어 |
| | `gateway` | `product: loxilb-inference-gateway` |

## 한계

- 헬스체크 이하 수준(TCP 도달성, 실제 트래픽)은 검증하지 않는다. 그건 loxilb 리포지토리의
  `cicd/` 시나리오(`k3s-base-sanity` 등)가 담당한다.
- 단일 노드 클러스터라 노드 IP 기반 엔드포인트는 변하지 않는다. 엔드포인트 증감은
  `f11-podnet` 처럼 pod 네트워크를 쓰는 서비스로만 관측된다.
- 가짜 loxilb 는 요청을 기록만 할 뿐 검증하지 않는다. **비교 대상이 되는 기준 바이너리가
  반드시 필요하다.**
