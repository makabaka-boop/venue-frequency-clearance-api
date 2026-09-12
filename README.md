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
| 极端数值 | 区间计算采用饱和算术：int64 极值中心频率不会回绕成颠倒区间，必判越界；超出 int64 的 JSON 数字按 400 字段错误拒绝 |

输出约定：

- 放行时 `devices` 按设备编号字典序升序输出保护区间。
- 拒绝时 `out_of_band` 按编号升序；`conflicts` 中每对的两个编号先按字典序排列
  （`first < second`），再按 `first`、`second` 升序去重输出。
- 越界设备同样参与冲突检测，两类问题一次性全部报告。

## 频率漂移净空（clearance）

放行只是"当下不冲突"。协调员若想知道方案对频率漂移还有多少余量、哪台设备最脆弱，
可在请求中带上可选开关 `include_clearance`（JSON 布尔值，省略或 `false` 表示关闭）。
**仅在 `accepted=true` 时**响应追加 `clearance` 字段；拒绝时仍只给出
`out_of_band` 与 `conflicts`。开关关闭时响应逐字段与不加开关完全一致。

净空以整数 kHz 计算，每台设备取下列候选中的最小值：

| 候选 | 含义 | limiter 标识 |
| --- | --- | --- |
| 频段余量（下） | 保护区间到下端点的距离：`low_khz − 470000` | `"band_low"` |
| 频段余量（上） | 保护区间到上端点的距离：`694000 − high_khz` | `"band_high"` |
| 相邻余量 | 与频率相邻的左/右保护区间之间**未占用的整数刻度数**：`邻.low_khz − 本.high_khz − 1` | 邻居设备的 `id` |

- 同值时按 limiter 标识的**字典序**取最小者（如 `"band_high" < "band_low"`，邻居编号按字符串比较）。
- `clearance.minimum_khz` 为全部设备最小值中的最小值（全局最脆弱处）；
  `clearance.devices` 按设备编号升序，每项含 `id`、`minimum_khz`、`limiter`。
- 端点压线放行时余量为 0：保护区间贴频段端点，或与邻居仅隔 1 kHz（中间 0 个空闲刻度）。
- 净空计算与区间计算一样使用饱和算术，极端中心频率不会回绕出颠倒的余量。
- `include_clearance` 不是布尔值（`null`、字符串、数字等）时返回 400，
  错误字段定位为 `include_clearance`，与设备级错误一并报告。

## API

### `POST /v1/coordinate`

请求体（`include_clearance` 可选，默认关闭）：

```json
{
  "include_clearance": true,
  "devices": [
    {"id": "alpha", "purpose": "handheld", "center_khz": 500000, "bandwidth_khz": 200}
  ]
}
```

响应：

| 情形 | HTTP | 正文 |
| --- | --- | --- |
| 放行 | 200 | `{"accepted": true, "devices": [{"id","low_khz","high_khz"}, ...]}`；请求带 `include_clearance=true` 时追加 `"clearance": {"minimum_khz", "devices": [{"id","minimum_khz","limiter"}, ...]}` |
| 越界 / 冲突 | 200 | `{"accepted": false, "out_of_band": [...], "conflicts": [{"first","second"}, ...]}`（即使请求了开关也不含 `clearance`） |
| 输入非法 | 400 | `{"accepted": false, "errors": [{"field","message"}, ...]}`，字段定位如 `devices[2].bandwidth_khz`、`include_clearance` |

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

### 6. 净空：单设备受频段边界限制

- `solo`：handheld，500000 / 200 → 保护区间 `[499775, 500225]`
- 频段余量：下 `499775 − 470000 = 29775`，上 `694000 − 500225 = 193775`；无邻居
- 最小值 29775 由 `band_low` 决定，全局最小值同为 29775

```console
$ curl -s -X POST http://localhost:8080/v1/coordinate -H 'Content-Type: application/json' -d '{
  "include_clearance": true,
  "devices": [
    {"id": "solo", "purpose": "handheld", "center_khz": 500000, "bandwidth_khz": 200}
  ]
}'
{"accepted":true,"clearance":{"minimum_khz":29775,"devices":[{"id":"solo","minimum_khz":29775,"limiter":"band_low"}]},"devices":[{"id":"solo","low_khz":499775,"high_khz":500225}]}
```

### 7. 净空：乱序三设备由中间邻接间隙决定

- `mic-a`：handheld，500000 / 200 → `[499775, 500225]`
- `mic-b`：handheld，500500 / 200 → `[500275, 500725]`
- `mic-c`：ifb，600000 / 201 → `[599650, 600351]`

按频率排序为 a、b、c。相邻余量：a↔b 之间空闲刻度 `500275 − 500225 − 1 = 49`，
b↔c 之间 `599650 − 500725 − 1 = 98924`。`mic-c` 的上频段余量 `694000 − 600351 = 93649`
小于其相邻余量，故它由 `band_high` 限制；全局最小值 49 来自中间的 a↔b 间隙。
提交顺序乱序（c、a、b）不影响结果，重复请求逐字节一致：

```console
$ curl -s -X POST http://localhost:8080/v1/coordinate -H 'Content-Type: application/json' -d '{
  "include_clearance": true,
  "devices": [
    {"id": "mic-c", "purpose": "ifb",      "center_khz": 600000, "bandwidth_khz": 201},
    {"id": "mic-a", "purpose": "handheld", "center_khz": 500000, "bandwidth_khz": 200},
    {"id": "mic-b", "purpose": "handheld", "center_khz": 500500, "bandwidth_khz": 200}
  ]
}'
{"accepted":true,"clearance":{"minimum_khz":49,"devices":[{"id":"mic-a","minimum_khz":49,"limiter":"mic-b"},{"id":"mic-b","minimum_khz":49,"limiter":"mic-a"},{"id":"mic-c","minimum_khz":93649,"limiter":"band_high"}]},"devices":[{"id":"mic-a","low_khz":499775,"high_khz":500225},{"id":"mic-b","low_khz":500275,"high_khz":500725},{"id":"mic-c","low_khz":599650,"high_khz":600351}]}
```

### 8. 开关类型错误（HTTP 400）

```console
$ curl -s -X POST http://localhost:8080/v1/coordinate -H 'Content-Type: application/json' -d '{
  "include_clearance": "yes",
  "devices": [
    {"id": "solo", "purpose": "handheld", "center_khz": 500000, "bandwidth_khz": 200}
  ]
}'
{"accepted":false,"errors":[{"field":"include_clearance","message":"must be a boolean, got \"yes\""}]}
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

一次性验收服务 `verify`：等待 API 就绪后执行 15 组端到端检查并逐条打印 PASS/FAIL，
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
internal/rules/           # 纯领域规则：保护区间、越界、冲突、裁决、漂移净空
internal/rules/rules_test.go
internal/api/             # Gin HTTP 层：请求校验、字段级错误、可选字段与响应整形
internal/api/handler_test.go
Dockerfile                # 多阶段构建，同一镜像提供 server 与 verify
docker-compose.yml        # api（API_PORT 可覆盖宿主端口）+ verify（一次性）
```
