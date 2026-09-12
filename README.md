# wireless-coordinator

多场会议共用场馆的无线话筒频率协调 API。协调员提交一批设备，服务返回**唯一的放行结论**：
要么 `accepted=true` 并给出每台设备的保护区间，要么 `accepted=false` 并给出稳定、可逐项复核的
越界设备清单与冲突对。

纯后端，无数据库、无外部状态；对同一份输入永远返回同一份结论。

## 协调规则

| 项目 | 规则 |
| --- | --- |
| 设备数量 | 1 至 200 台，`id` 为唯一非空字符串 |
| 用途 `purpose` | `handheld` / `bodypack` / `ifb`，两侧保护间隔固定为 **125 / 175 / 250 kHz** |
| 带宽 `bandwidth_khz` | 25 至 400 的正整数（kHz） |
| 中心频率 `center_khz` | 整数（kHz） |
| 占用区间 | `[center − floor(bw/2), center + ceil(bw/2)]`（闭区间） |
| 保护区间 | 占用区间向两侧各扩展该用途的保护间隔 |
| 允许频段 | 闭区间 `[470000, 694000]` kHz，端点压线算在界内 |
| 冲突判定 | 任意两台设备的**保护区间**相交即冲突；闭区间**端点相等也算冲突** |
| 裁决 | 任一保护区间越界或存在任一冲突对，**整份拒绝** |

输出约定：

- 放行时 `devices` 按设备编号字典序升序输出保护区间。
- 拒绝时 `out_of_band` 按编号升序；`conflicts` 中每对的两个编号先按字典序排列
  （`first < second`），再按 `first`、`second` 升序去重输出。
- 越界设备同样参与冲突检测，两类问题一次性全部报告。

## API

### `POST /v1/coordinate`

请求体：

```json
{
  "devices": [
    {"id": "alpha", "purpose": "handheld", "center_khz": 500000, "bandwidth_khz": 200}
  ]
}
```

响应：

| 情形 | HTTP | 正文 |
| --- | --- | --- |
| 放行 | 200 | `{"accepted": true, "devices": [{"id","low_khz","high_khz"}, ...]}` |
| 越界 / 冲突 | 200 | `{"accepted": false, "out_of_band": [...], "conflicts": [{"first","second"}, ...]}` |
| 输入非法 | 400 | `{"accepted": false, "errors": [{"field","message"}, ...]}`，字段定位如 `devices[2].bandwidth_khz` |

另提供 `GET /healthz` 用于健康检查。

## 可复算示例

以下每条都给出手工推导，可用 `curl` 复算核对。

### 1. 放行（含奇数带宽）

- `alpha`：handheld，中心 500000，带宽 200 → 占用 `[500000−100, 500000+100] = [499900, 500100]`，
  两侧各 125 → 保护区间 **[499775, 500225]**
- `gamma`：ifb，中心 600000，带宽 201 → `floor(201/2)=100`、`ceil(201/2)=101`，
  占用 `[599900, 600101]`，两侧各 250 → 保护区间 **[599650, 600351]**

两区间不相交，且都在 `[470000, 694000]` 内：

```console
$ curl -s -X POST http://localhost:8080/v1/coordinate -H 'Content-Type: application/json' -d '{
  "devices": [
    {"id": "gamma", "purpose": "ifb",       "center_khz": 600000, "bandwidth_khz": 201},
    {"id": "alpha", "purpose": "handheld",  "center_khz": 500000, "bandwidth_khz": 200}
  ]
}'
{"accepted":true,"devices":[{"id":"alpha","low_khz":499775,"high_khz":500225},{"id":"gamma","low_khz":599650,"high_khz":600351}]}
```

注意提交顺序是 `gamma, alpha`，输出按编号排序为 `alpha, gamma`。

### 2. 冲突（载波不重叠，但保护间隔不足）

- `alpha`：handheld，500000 / 200 → 保护区间 `[499775, 500225]`
- `beta`：bodypack，500300 / 100 → 占用 `[500250, 500350]`，两侧各 175 → 保护区间 `[500075, 500525]`

两台载波占用区间并不重叠（500100 < 500250），但 `500075 ≤ 500225`，保护区间相交：

```console
$ curl -s -X POST http://localhost:8080/v1/coordinate -H 'Content-Type: application/json' -d '{
  "devices": [
    {"id": "beta",  "purpose": "bodypack",  "center_khz": 500300, "bandwidth_khz": 100},
    {"id": "alpha", "purpose": "handheld",  "center_khz": 500000, "bandwidth_khz": 200}
  ]
}'
{"accepted":false,"conflicts":[{"first":"alpha","second":"beta"}],"out_of_band":[]}
```

### 3. 端点相触也算冲突

两台 handheld、带宽 200 的设备，保护区间都是 `center ± 225`。中心相距 450 kHz 时，
`a` 的上端 `500225` 恰好等于 `b` 的下端 `500225` → 冲突；相距 451 kHz 则放行。

### 4. 越界即整份拒绝

- `low`：ifb，470200 / 100 → 占用 `[470150, 470250]`，两侧各 250 → `[469900, 470500]`，低于 470000。

```console
$ curl -s -X POST http://localhost:8080/v1/coordinate -H 'Content-Type: application/json' -d '{
  "devices": [
    {"id": "low", "purpose": "ifb",      "center_khz": 470200, "bandwidth_khz": 100},
    {"id": "ok",  "purpose": "handheld", "center_khz": 500000, "bandwidth_khz": 200}
  ]
}'
{"accepted":false,"conflicts":[],"out_of_band":[{"id":"low","low_khz":469900,"high_khz":470500}]}
```

边界是闭区间：handheld、带宽 25、中心 470137 时保护区间下端恰好 `470000`，算界内；
中心 470136 则越界。

### 5. 可定位字段的错误反馈（HTTP 400）

```console
$ curl -s -X POST http://localhost:8080/v1/coordinate -H 'Content-Type: application/json' -d '{
  "devices": [
    {"id": "mic-1", "purpose": "lavalier", "center_khz": 500000, "bandwidth_khz": 401}
  ]
}'
{"accepted":false,"errors":[{"field":"devices[0].purpose","message":"must be one of \"handheld\", \"bodypack\", \"ifb\""},{"field":"devices[0].bandwidth_khz","message":"must be between 25 and 400 kHz, got 401"}]}
```

## 运行

### 本地（Go 1.25+）

```console
$ go test ./...        # 规则包旁的单元测试 + HTTP 层测试
$ go run ./cmd/server  # 监听 :8080，可用 PORT 覆盖
```

### Docker Compose

```console
$ docker compose up --build api                 # 默认宿主端口 8080
$ API_PORT=9000 docker compose up --build api   # API_PORT 覆盖宿主端口
```

一次性验收服务 `verify`：等待 API 就绪后执行 9 组端到端检查并逐条打印 PASS/FAIL，
任一失败则以非零码退出：

```console
$ docker compose up --build --exit-code-from verify verify
```

也可对任意已运行的实例执行：`go run ./cmd/verify`（默认 `http://localhost:8080`，
可用 `API_URL` 覆盖）。

## 项目结构

```
cmd/server/main.go        # 服务入口（PORT，默认 8080）
cmd/verify/main.go        # 一次性验收程序
internal/rules/           # 纯领域规则：保护区间、越界、冲突、裁决
internal/rules/rules_test.go
internal/api/             # Gin HTTP 层：请求校验、字段级错误、响应整形
internal/api/handler_test.go
Dockerfile                # 多阶段构建，同一镜像提供 server 与 verify
docker-compose.yml        # api（API_PORT 可覆盖宿主端口）+ verify（一次性）
```
