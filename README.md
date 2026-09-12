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
| 带宽 `bandwidth_khz` | 25 至 400 的正整数（kHz）；MHz 模式下经定点换算后仍须落在该范围 |
| 中心频率 `center_khz` | 整数（kHz）；MHz 模式下为最多三位小数的 MHz 数字 |
| 频率单位 `frequency_unit` | 顶层可选；省略或 `"mhz"`。省略时 `center_khz` / `bandwidth_khz` 只接受整数 kHz；`"mhz"` 时接受最多三位小数的 MHz JSON 数字，HTTP 层按**十进制定点**精确换算成整数 kHz（`0.201 → 201`），再交给既有区间裁决与净空计算，响应一律仍输出 kHz |
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

### 禁止重复字段

JSON 标准允许对象内同名键重复出现，其语义未定义；编码库默认"静默取后值"，
会让一份含歧义的请求按后值裁决。本服务对**同一对象内的任何重复键一律以 400 拒绝**，
错误定位到重复字段本身（`field` 取**第二次出现**的路径），即使两次取值相同也拒绝：

- 请求顶层同时写两个 `include_clearance`（如先 `false` 后 `true`）→ 定位 `include_clearance`；
- 请求顶层同时写两份 `devices` → 定位 `devices`，不存在"只裁决最后一份"；
- 单台设备对象内重复字段（如两个 `center_khz`、两个 `bandwidth_khz`）→
  定位 `devices[i].center_khz` 等。

重复字段属请求结构错误，先于设备级校验报告；一份请求中的多个重复字段一次性全部列出。

## MHz 直填（`frequency_unit`）

场馆设备清单常按 MHz 小数登记中心频率与带宽（如 500.125 MHz、0.201 MHz）。协调员可在请求
顶层带 `"frequency_unit": "mhz"`，直接提交原始数字，无需人工换算成 kHz：

- `center_khz` / `bandwidth_khz` 接受**最多三位小数**的 JSON 数字（`500`、`500.0`、`0.201` 均可），
  字段名仍为 `*_khz`，但数值按 MHz 解释。
- HTTP 层用**十进制定点**算术乘 1000 精确换算为整数 kHz（如 `500.125 → 500125`、`0.201 → 201`），
  全程不经过浮点，杜绝 `0.1 + 0.2` 式偏差。
- 换算在进入领域规则前完成：之后的占用/保护区间、设备排序、冲突判定、净空及其 limiter 来源
  **不另设 MHz 分支**，与提交等价整数 kHz 的请求**逐字节一致**；响应始终输出 kHz。
- 省略 `frequency_unit` 时既有整数 kHz 契约完全不变，成功与拒绝响应逐字节保持原状。
  取值只有"省略"与 `"mhz"` 两种合法形态：显式写 `"khz"` 或其他单位会被拒绝。

MHz 模式下下列情况返回定位到设备属性的 400（与其他设备级错误一并报告）：

| 问题 | 定位字段 |
| --- | --- |
| 小数位超过三位（如 `500.1234`） | `devices[i].center_khz` / `devices[i].bandwidth_khz` |
| 指数记法（如 `5e2`，非十进制定点） | 同上 |
| 换算结果超出 int64 kHz 范围 | 同上 |
| 换算后的带宽不在既有 25–400 kHz 范围 | `devices[i].bandwidth_khz`（错误文案给出换算后的 kHz 整数） |

`frequency_unit` 本身取值未知（`"ghz"`、大小写不符的 `"MHz"`、显式 `"khz"` 等）、类型错误
（`null`、数字、布尔等），或在同一对象内重复出现时，在裁决前以定位 `frequency_unit` 的 400 拒绝。

## 试算调谐（`check-retunes`）

协调员拿到冲突结论后，往往已有若干备选中心频率，希望在改动设备清单前一次判断哪个调谐值能
恢复放行。`POST /v1/check-retunes` 接收现有 `devices`、唯一的 `target_id` 和 1 至 50 个
`candidate_centers_khz`，把每个候选**替换目标设备的中心频率**后交给既有区间裁决——
其余设备以及目标设备的带宽、用途均不改动。

- `devices` 与 `frequency_unit` 的解析、校验和 `/v1/coordinate` 完全一致；候选中心频率复用
  同一套整数 kHz / MHz 定点解析，响应一律输出 kHz。
- 结果按**换算后的中心频率升序**返回，每项含 `center_khz`、`accepted`；被拒绝的候选追加该次
  试算**完整的** `out_of_band` 与 `conflicts`（结构与协调接口的拒绝响应相同）。
  候选顺序、设备顺序不影响输出，重复提交逐字节一致。
- 合法候选导致保护区间越界属于**试算结论**（200，`accepted:false` 并携带越界明细），
  不作为请求错误。

下列情况返回定位到字段的 400（与设备级错误一并报告）：

| 问题 | 定位字段 |
| --- | --- |
| `target_id` 缺失、不是字符串、为空或不存在于设备清单 | `target_id` |
| 候选数组缺失、为空或超过 50 个 | `candidate_centers_khz` |
| 候选数值精度非法（kHz 非整数、MHz 超三位小数、指数记法）或换算超出 int64 | `candidate_centers_khz[i]` |
| 候选归一化（换算成 kHz）后重复（如 MHz 下 `500` 与 `500.000`） | `candidate_centers_khz[j]`（后一次出现） |
| 同一对象内重复键（两个 `target_id`、两份候选数组等） | 重复字段本身 |

## API

### `POST /v1/coordinate`

请求体（`include_clearance`、`frequency_unit` 均可选，默认关闭 / 整数 kHz）：

```json
{
  "include_clearance": true,
  "frequency_unit": "mhz",
  "devices": [
    {"id": "alpha", "purpose": "handheld", "center_khz": 500.0, "bandwidth_khz": 0.2}
  ]
}
```

响应：

| 情形 | HTTP | 正文 |
| --- | --- | --- |
| 放行 | 200 | `{"accepted": true, "devices": [{"id","low_khz","high_khz"}, ...]}`；请求带 `include_clearance=true` 时追加 `"clearance": {"minimum_khz", "devices": [{"id","minimum_khz","limiter"}, ...]}` |
| 越界 / 冲突 | 200 | `{"accepted": false, "out_of_band": [...], "conflicts": [{"first","second"}, ...]}`（即使请求了开关也不含 `clearance`） |
| 输入非法 | 400 | `{"accepted": false, "errors": [{"field","message"}, ...]}`，字段定位如 `devices[2].bandwidth_khz`、`include_clearance`、`frequency_unit`；同一 JSON 对象内重复键（如两份 `devices`、两个 `include_clearance`、设备内两个 `center_khz`、两个 `frequency_unit`）同样 400，定位到重复字段 |

### `POST /v1/check-retunes`

请求体（`frequency_unit` 可选，语义同 `/v1/coordinate`）：

```json
{
  "devices": [
    {"id": "other",  "purpose": "handheld", "center_khz": 500450, "bandwidth_khz": 200},
    {"id": "target", "purpose": "handheld", "center_khz": 500000, "bandwidth_khz": 200}
  ],
  "target_id": "target",
  "candidate_centers_khz": [500450, 499000, 500000]
}
```

响应：

| 情形 | HTTP | 正文 |
| --- | --- | --- |
| 试算完成 | 200 | `{"results": [{"center_khz","accepted"}, ...]}`，按换算后中心频率升序；被拒绝的候选追加 `"out_of_band": [...]`、`"conflicts": [...]` |
| 输入非法 | 400 | `{"accepted": false, "errors": [{"field","message"}, ...]}`，字段定位如 `target_id`、`candidate_centers_khz[2]` |

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

### 9. 重复字段一律拒绝（HTTP 400，不采用后值）

净空开关同时写关闭和开启时，接口不会静默采用后值并返回净空：

```console
$ curl -s -X POST http://localhost:8080/v1/coordinate -H 'Content-Type: application/json' -d '{
  "include_clearance": false,
  "include_clearance": true,
  "devices": [
    {"id": "solo", "purpose": "handheld", "center_khz": 500000, "bandwidth_khz": 200}
  ]
}'
{"accepted":false,"errors":[{"field":"include_clearance","message":"appears more than once in the same JSON object; \"include_clearance\" must be provided at most once"}]}
```

同理，误填两份不同 `devices` 列表只裁决最后一份的情况不会发生——重复列表以
`"field":"devices"` 拒绝；单台设备给两个不同 `center_khz` 则以
`"field":"devices[0].center_khz"` 拒绝，不按最后一个频率计算保护区间。

### 10. MHz 直填与整数 kHz 等价（奇数带宽）

`"frequency_unit":"mhz"` 下，`gamma` 的 600 MHz / 0.201 MHz 先定点换算为 600000 / 201 kHz
（奇数带宽的非对称占用区间照常），`alpha` 的 500 MHz / 0.2 MHz 换算为 500000 / 200 kHz，
随后的保护区间、排序与净空与示例 1、7 完全相同，响应仍以 kHz 输出：

```console
$ curl -s -X POST http://localhost:8080/v1/coordinate -H 'Content-Type: application/json' -d '{
  "frequency_unit": "mhz",
  "include_clearance": true,
  "devices": [
    {"id": "gamma", "purpose": "ifb",      "center_khz": 600,    "bandwidth_khz": 0.201},
    {"id": "alpha", "purpose": "handheld", "center_khz": 500.000, "bandwidth_khz": 0.200}
  ]
}'
{"accepted":true,"clearance":{"minimum_khz":29775,"devices":[{"id":"alpha","minimum_khz":29775,"limiter":"band_low"},{"id":"gamma","minimum_khz":93649,"limiter":"band_high"}]},"devices":[{"id":"alpha","low_khz":499775,"high_khz":500225},{"id":"gamma","low_khz":599650,"high_khz":600351}]}
```

该响应与提交等价整数 kHz（不带 `frequency_unit`）的响应逐字节一致；MHz 模式下端点相触
（500.000 与 500.450 MHz）同样判冲突。非法精度可定位到设备属性：

```console
$ curl -s -X POST http://localhost:8080/v1/coordinate -H 'Content-Type: application/json' -d '{
  "frequency_unit": "mhz",
  "devices": [
    {"id": "a", "purpose": "handheld", "center_khz": 500.1234, "bandwidth_khz": 0.2}
  ]
}'
{"accepted":false,"errors":[{"field":"devices[0].center_khz","message":"must be a MHz number with at most three decimal places converting to an integer kHz, got 500.1234"}]}
```

`"frequency_unit":"khz"`、`"GHz"`、`null`、数字等未知或类型错误的取值，以及顶层重复的
`frequency_unit`，都在裁决前以定位 `frequency_unit` 的 400 拒绝。不带 `frequency_unit` 时，
整数 kHz 的既有成功与拒绝响应逐字节不变。

### 11. 试算调谐：一个候选解除目标冲突

- `other`：handheld，500450 / 200 → 保护区间 `[500225, 500675]`
- `target`：handheld，500000 / 200 → 保护区间 `[499775, 500225]`，与 `other` 端点相触 → 冲突
- 候选 `499000` → `[498775, 499225]`，与 `other` 不再相交 → 放行；
  候选 `500450` 与 `other` 同频 → 冲突

```console
$ curl -s -X POST http://localhost:8080/v1/check-retunes -H 'Content-Type: application/json' -d '{
  "devices": [
    {"id": "other",  "purpose": "handheld", "center_khz": 500450, "bandwidth_khz": 200},
    {"id": "target", "purpose": "handheld", "center_khz": 500000, "bandwidth_khz": 200}
  ],
  "target_id": "target",
  "candidate_centers_khz": [500450, 499000, 500000]
}'
{"results":[{"center_khz":499000,"accepted":true},{"center_khz":500000,"accepted":false,"out_of_band":[],"conflicts":[{"first":"other","second":"target"}]},{"center_khz":500450,"accepted":false,"out_of_band":[],"conflicts":[{"first":"other","second":"target"}]}]}
```

候选乱序提交、设备乱序提交，输出都逐字节不变；`"frequency_unit":"mhz"` 下候选写
`500.450`、`499.000`、`500.000` 时响应与本例逐字节一致。目标编号不存在、候选数组为空、
候选精度非法或归一化后重复（如 MHz 下 `500` 与 `500.000`）均返回定位到字段的 400；
而候选 `470000` 这类合法数值导致保护区间越界时，结论体现在该候选的
`accepted:false` 与 `out_of_band` 明细中，请求本身仍是 200。

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

一次性验收服务 `verify`：等待 API 就绪后执行 26 组端到端检查并逐条打印 PASS/FAIL，
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
