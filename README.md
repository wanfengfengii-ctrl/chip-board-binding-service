# binding-service

烧录工位芯片/板卡一对一绑定服务。纯后端，Go 1.25 + Gin + PostgreSQL。

工位把芯片焊上板卡后调用本服务登记两者关系。服务保证：

- **幂等**：客户端生成的请求键 + 完全相同载荷重放时返回首次的原始结果（`200`），不会产生第二条记录。
- **请求键冲突**：同一请求键携带不同载荷 → `409 REQUEST_KEY_CONFLICT`，响应体附带已存在的原始记录。
- **器件占用**：不同请求键引用任一已绑定的芯片 UID 或板卡号 → `409 DEVICE_ALREADY_BOUND`，并指明冲突字段（`chip_uid` / `board_serial`）。
- **不可改写**：既有记录永不更新、永不删除；被拒绝的请求整次回滚，不留痕迹。
- **并发安全只靠数据库**：唯一约束 + 单事务写入，任意数量的服务副本和工位争用同一标识时只有一个成功，不依赖任何进程内锁。

## 数据模型

一次成功事务同时写入两张表（`internal/db/migrations/0001_init.sql`）：

| 表 | 作用 | 关键约束 |
|---|---|---|
| `requests` | 请求账本（幂等键） | `request_key` 主键 |
| `bindings` | 芯片↔板卡一对一绑定 | `request_key`、`chip_uid`、`board_serial` 各自唯一 |

并发争用时，`INSERT ... ON CONFLICT DO NOTHING` 会等待在飞事务落定：赢家提交后，同键重放读到原始记录；`bindings` 上的唯一冲突则精确指出哪个器件已被占用。三张表的标识列还有 `CHECK (col ~ '^[A-Z0-9-]{1,64}$')` 兜底，应用层校验之外数据库也拒绝非法标识。

`0002_inspections.sql` 增加返修拆机前的实物核验账本 `inspections`：保存扫描值 `chip_uid` / `board_serial`、判定结果 `result`、两侧可空的绑定编号（外键到 `bindings.id`）和创建时间。该表**只写不改**——除应用层不提供更新/删除入口外，数据库触发器 `inspections_no_update` / `inspections_no_delete` 会直接拒绝任何 UPDATE 或 DELETE。

`0003_mismatch_peers.sql` 为串件排查补充两个部分索引（仅覆盖 `result = 'MISMATCH'` 的行，分别以 `chip_binding_id` / `board_binding_id` 为前导列），让按绑定聚合不匹配核验只扫描触及目标绑定的记录。

## API

三个标识（`request_key`、`chip_uid`、`board_serial`）均为 1–64 位 ASCII 大写字母、数字或连字符；任一字段非法 → 整次 `422`，不入库。

所有 JSON 请求体还遵循一条**字段唯一性**规则：同一对象内每个成员名只能出现一次（即使两次取值完全相同）。Go 默认的 JSON 解码对重复键采用“末次值覆盖”，会让字段含义不唯一；服务在解码前先做流式校验，发现重复成员即整次拒绝（`422 VALIDATION_FAILED`），不会使用任何一个冲突值建档或判定。请求体包含两个并列 JSON 值（`}{` 拼接）同样整次拒绝。

### `POST /api/v1/bindings`

```json
{ "request_key": "REQ-001", "chip_uid": "CHIP-9", "board_serial": "BOARD-7" }
```

| 状态 | 含义 |
|---|---|
| `201` | 新建成功，返回绑定记录 |
| `200` | 同键同载荷重放，返回原始记录（与首次响应逐字节一致） |
| `409` | `REQUEST_KEY_CONFLICT`（同键不同载荷，附 `existing` 原始记录）或 `DEVICE_ALREADY_BOUND`（器件已占用，附 `field`） |
| `422` | 校验失败，整次拒绝；请求体重复携带任一字段（`request_key` / `chip_uid` / `board_serial`，即使值相同）同样整次拒绝，`error.details` 标出重复字段 |

### 查询（未找到统一 `404 NOT_FOUND`）

- `GET /api/v1/bindings/by-request-key/{request_key}`
- `GET /api/v1/bindings/by-chip-uid/{chip_uid}`
- `GET /api/v1/bindings/by-board-serial/{board_serial}`

路径中的标识同样适用 1–64 位标识规则：非法标识（小写、下划线、超长等）整次拒绝，返回 `422 VALIDATION_FAILED`，不查询数据库；只有合法但未登记的标识才返回 `404 NOT_FOUND`。

### `POST /api/v1/bindings/batch-lookup`

返修工位一次扫描多块板卡后的批量核对入口。提交 1–100 个带行号的查询项，每项按三种标识**三选一**：

```json
{
  "queries": [
    {"line": 1, "type": "chip_uid",     "value": "CHIP-9"},
    {"line": 2, "type": "board_serial", "value": "BOARD-7"},
    {"line": 3, "type": "request_key",  "value": "REQ-001"}
  ]
}
```

`type` 取值 `chip_uid` / `board_serial` / `request_key`；`value` 适用同样的 1–64 位标识规则；`line` 为正整数且同一批内不得重复。响应严格保持输入顺序，逐行回显 `line` / `type` / `value`：

```json
{
  "results": [
    {"line": 1, "type": "chip_uid", "value": "CHIP-9", "status": "FOUND",
     "binding": {"binding_id": 1, "request_key": "REQ-001", "chip_uid": "CHIP-9", "board_serial": "BOARD-7", "created_at": "..."}},
    {"line": 2, "type": "board_serial", "value": "BOARD-7", "status": "NOT_FOUND"}
  ]
}
```

- 命中返回 `FOUND` 并携带与单项查询一致的完整 Binding 结构；未命中返回 `NOT_FOUND`（单项缺失不影响其他行，也不写入请求账本）。
- 整批查询在**同一个只读 REPEATABLE READ 事务快照**中完成，核对期间其他工位并发绑定不会让同一批结果来自不同时间点。
- 相同 `(type, value)` 在存储层去重，只查询一次后还原到每个重复行；整批固定为 BEGIN / 单条 SELECT / COMMIT 三次数据库往返，**不随条目数线性增长**。
- 校验失败整批返回 `422 VALIDATION_FAILED`，`error.field_errors[]` 用 `queries[i].field` 形式标出每个出错位置；数据库故障返回 `500 INTERNAL`。

| 非法情形 | 位置 |
|---|---|
| `queries` 为空数组/缺失 | `queries` |
| 顶层重复携带 `queries`（结构不唯一，即使两组内容相同） | `queries` |
| 超过 100 项 | `queries` |
| 行号缺失、非正整数、重复 | `queries[i].line` |
| 未知查询类型 | `queries[i].type` |
| 单个查询项重复携带任一字段（`line` / `type` / `value`，语义不明确） | `queries[i].<字段>` |
| 非法标识 | `queries[i].value` |

### `GET /healthz`

存活探针（含数据库连通性检查）。

## 返修实物核验

返修人员拆机前同时扫描芯片 UID 与板卡序列号，服务在**一次数据库事务**中按现有绑定关系解析两端，并把这次实物核验作为不可修改的检查记录保存。

### `POST /api/v1/inspections`

```json
{ "chip_uid": "CHIP-9", "board_serial": "BOARD-7" }
```

两个标识适用同样的 1–64 位标识规则；非法标识整次 `422 VALIDATION_FAILED`，不落库。请求体重复携带 `chip_uid` 或 `board_serial`（即使两次扫描值相同）同样整次 `422 VALIDATION_FAILED`，`error.details` 标出重复字段——不能按末次扫描值保存判定。事务先在同一快照中解析两侧命中的绑定（一条 SQL 的两个 LATERAL 子查询），再按命中关系写入核验记录：

| 判定 | 条件 |
|---|---|
| `CONSISTENT` | 两侧都命中且是**同一条**绑定 |
| `MISMATCH` | 两侧都命中但是**不同**绑定（交叉绑定） |
| `PARTIAL` | 仅一侧命中 |
| `UNREGISTERED` | 两侧都未登记 |

`201` 响应与记录结构：

```json
{
  "inspection_id": 1,
  "chip_uid": "CHIP-9",
  "board_serial": "BOARD-7",
  "result": "MISMATCH",
  "chip_binding_id": 1,
  "board_binding_id": 2,
  "created_at": "...",
  "chip_binding":  { "binding_id": 1, "request_key": "REQ-001", "chip_uid": "CHIP-9", "board_serial": "BOARD-1", "created_at": "..." },
  "board_binding": { "binding_id": 2, "request_key": "REQ-002", "chip_uid": "CHIP-2", "board_serial": "BOARD-7", "created_at": "..." }
}
```

- `chip_binding_id` / `board_binding_id` 两侧可空（未命中为 `null`）；命中时额外返回该侧命中绑定的完整摘要 `chip_binding` / `board_binding`，未命中字段省略。
- 核验只读现有绑定并追加核验记录，不写 `requests` 账本、不改变任何既有绑定。
- 解析或保存失败返回 `500 INTERNAL` 错误信封，事务回滚不留记录。

### `GET /api/v1/inspections/{inspection_id}`

返修人员复核原始判定，返回与创建时一致的记录结构。

- 路径段必须是正整数；`0`、负数、小数、带符号或非数字一律 `422 VALIDATION_FAILED`，不查询数据库。
- 合法但不存在的编号返回 `404 NOT_FOUND`。
- 记录写入后不提供更新或删除入口，数据库触发器同样拒绝任何 UPDATE / DELETE。

## 串件排查

返修主管发现某条绑定疑似串件时，不必逐条翻阅检查记录：下面的端点直接给出它在不匹配核验中牵连的其他绑定，按重复发生程度排序，便于安排排查。

### `GET /api/v1/bindings/{binding_id}/mismatch-peers?limit=`

```json
{
  "binding": { "binding_id": 1, "request_key": "REQ-001", "chip_uid": "CHIP-9", "board_serial": "BOARD-7", "created_at": "..." },
  "peers": [
    {
      "binding": { "binding_id": 2, "request_key": "REQ-002", "chip_uid": "CHIP-2", "board_serial": "BOARD-1", "created_at": "..." },
      "occurrences": 3,
      "last_inspection_id": 42,
      "last_occurred_at": "..."
    }
  ]
}
```

- 返回目标绑定摘要 `binding` 与关联绑定列表 `peers`；每个关联项携带该侧绑定摘要、共同出现次数 `occurrences`、最近检查编号 `last_inspection_id` 和最近发生时间 `last_occurred_at`。
- 聚合只依据不可修改核验记录上保存的两侧绑定编号（`result = 'MISMATCH'` 的记录），不按当前标识重新解析——后续绑定数据变化不会重算历史关系；`CONSISTENT` / `PARTIAL` / `UNREGISTERED` 核验不参与统计。
- 排序固定为次数降序、最近检查编号降序、关联绑定编号升序，截取稳定；`limit` 可选，1–50，缺省 50。
- 整个查询在**同一个只读 REPEATABLE READ 事务**中完成，全程不产生写入；`0003_mismatch_peers.sql` 为两侧绑定编号各建了一个仅匹配 `MISMATCH` 行的部分索引。
- 没有不匹配记录时 `peers` 为空数组 `[]`（目标绑定存在时仍返回 `200`）。

| 情形 | 响应 |
|---|---|
| `binding_id` 非正整数（`0`、负数、小数、带符号、非数字） | `422 VALIDATION_FAILED`，`details.binding_id`，不查询数据库 |
| `limit` 越界或非整数（`0`、`51`、`abc` 等） | `422 VALIDATION_FAILED`，`details.limit`，不查询数据库 |
| 目标绑定不存在 | `404 NOT_FOUND` |
| 数据库故障 | `500 INTERNAL` |

## 运行（Docker Compose）

```bash
docker compose up --build api          # 默认宿主端口 8080
API_PORT=9090 docker compose up api    # API_PORT 覆盖宿主端口
```

服务：`db`（PostgreSQL 16，不暴露宿主端口）、`api`（迁移随启动自动执行）、`verify`（一次性验收）。

## 验收

`verify` 是一次性验收服务，模拟产线真实故障模式：首次成功后响应丢失并重放、两个工位同时争用同一芯片/板卡、同键重放竞速、同键异载荷竞速、非法输入拒绝、重复字段整次拒绝（建档字段重复、核验扫描字段重复、批量查询项类型重复、顶层查询数组重复），以及返修实物核验的四种判定（同一绑定 `CONSISTENT`、交叉绑定 `MISMATCH`、单侧命中 `PARTIAL`、双侧未命中 `UNREGISTERED`）与复核/错误契约。每次运行使用唯一标识后缀，可重复执行。

```bash
docker compose up --build --exit-code-from verify
# 退出码 0 = VERIFY PASSED，非 0 = 有检查失败
```

## 测试

testify 集成测试覆盖并发争用、重放、重启后持久化（真实 PostgreSQL，非占位实现）：

```bash
TEST_DATABASE_URL='postgres://postgres:postgres@localhost:5432/postgres?sslmode=disable' go test ./... -count=1 -race
```

未设置 `TEST_DATABASE_URL` 时集成测试跳过，纯单元测试（标识校验）仍会运行。

## 环境变量

| 变量 | 服务 | 默认 | 说明 |
|---|---|---|---|
| `DATABASE_URL` | api | （必填） | PostgreSQL 连接串 |
| `HTTP_ADDR` | api | `:8080` | 监听地址 |
| `API_PORT` | compose | `8080` | 映射到宿主的端口 |
| `API_BASE_URL` | verify | `http://localhost:8080` | 被测 API 地址 |
| `TEST_DATABASE_URL` | 测试 | （空则跳过集成测试） | 测试数据库连接串 |

## 结构

```
cmd/server    API 服务入口（优雅退出）
cmd/verify    一次性验收客户端
internal/binding  领域层：校验、存储（事务+唯一约束）、HTTP 处理
internal/config   环境变量配置
internal/db       连接池与嵌入式 SQL 迁移
```
