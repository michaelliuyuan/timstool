# MS-08 · 比对面 MySQL 源接入（方言分叉全账）

> 基线 e88b10e（V3.17，件 92A1D1D3）。链：`29b4a78`①芯 → `bd788ce`②闸 → docs → config 锚。
> 裁定：leader 清单票 v2 定稿 seq410（红队 :572 传播缺口坐实 seq407、positive 预研 seq404-409）；行动项 a 实测（TiDB 5000 充 MySQL）原文 seq412。

## 交付总览（用户可见）

比对页对 MySQL 源解灰开放：source_ref 与 inline 两路 mysql 源过闸 → validator 以 mysqlDialect（MySQL wire）执行 quick/sample/checksum/watermark 全模式比对，目标恒 TiDB。

## ① 芯（29b4a78）——装配+传播+方言

| 位置 | 内容 |
| --- | --- |
| validator.go NewValidator | srcDialect 按 `cfg.Source.SourceType()` 装配（空默认 postgres 恒等；unknown→PG 形态不可达注释）+srcDriverName()（pgx/mysql） |
| validator.go :208 | `sql.Open(v.srcDriverName(), pgDSN)` |
| compare.go :571/:590 | **红队 :572 传播修（案一）**：`NewValidator(config.Config{Source: task.Source})`（Type 随 cfg 单一真源）+`task.Source.DSNByType()`；RunWithDSNs 签名零侵入 |
| config.go DSNByType | 新增按 SourceType() 分派：PG=DSN() 恒等；mysql=BuildMySQLDSN（loc=UTC、charset utf8mb4、10s 连接超时） |
| dialect_mysql.go | mysqlDialect 9 方法（QuoteIdent 反引号/Qualify 两段 `s`.`t`/EstimateRows=SHOW TABLE STATUS FROM schema/CountExact/WmPredicateFragment `?`/ValidateWatermarkColumn（DATA_TYPE 白名单 datetime·timestamp·date·int·bigint）/DetectTableKey（information_schema.statistics，PRIMARY→unique 序）/AdjustDSN=no-op（Loc=UTC 已在 DSN builder）/ListTables `?`） |
| dialect.go 接口 | CompareDialect 增 ListTables（占位符方言属性；PG 实现恒等搬迁 getTables 探测） |
| validator.go sourceSchema | mysql 空 schema→连接 Database（非 public） |
| validator.go normalizeValue | uint64 case（dialect.go:22 seam 单共享扩展，PG 不可达恒等） |

**行动项 a 实测结论**（TiDB 5000 充 MySQL，fixture pggate_ms08 自清）：文本协议下 UNSIGNED BIGINT/VARCHAR/DECIMAL 恒 []byte（uint64 仅二进制/interpolate 路径，case 属防御）；DATETIME→time.Time UTC（ParseTime）；DATA_TYPE 小写形态；SHOW TABLE STATUS 跨库需 `FROM schema` 形态（LIKE 裸表名在未 USE 时 no rows 实测）。

### normalizeValue 分叉点账（值形态正确性核心）

| 差异点 | 处置 |
| --- | --- |
| 时间 | 两 driver 均 time.Time UTC → 共享 case 恒等（DATETIME 无时区折算：loc=UTC 读即写值）；normalizeTimestampString 剥 fractional/timezone 已共享 |
| DECIMAL | 两侧文本（[]byte/string）→ normalizeDecimalString 剥尾零已共享 |
| UNSIGNED BIGINT | 文本 []byte（主路径）/uint64（防御 case）→ 交叉等值锚钉 |
| CHAR 尾随空格 | 列级 trimCols 机制（checksum.go）双侧已共享，MySQL 源侧自然继承 |
| `\r` 行尾 | normalizeString 归一已共享 |
| zero-date `0000-00-00` | **显式 fail**：driver Scan err 走 per-table tr.Error（带表名上下文）；TiDB 门禁 A 案覆盖不到（默认禁 zero-date），白盒形态记档 |
| BINARY `\0` | []byte→string 直通（与 PG bytea 同形态），差异场景记 MS-09+ 观察 |

**交叉等值锚**：TestNormalizeValuePGMySQLParity——同逻辑值 PG 扫描形态 vs MySQL 扫描形态断言逐字节相等（varchar/decimal±/datetime/int/unsigned-bigint/null）。

## ② 闸（bd788ce）——开闸点

| 位置 | legacy | 新 | 形态 |
| --- | --- | --- | --- |
| compare.go :393（source_ref） | `e.Type != "postgres"` | `!srcCapable(e.Type, CapCompare)` | C1 shape-1 原始值空=拒 |
| compare.go :423（inline） | `Type != "" && Type != "postgres"` | `!srcCapable(req.Source.SourceType(), CapCompare)` | C1 shape-2 归一空=postgres=放行（与 legacy 空=放恒等） |

- 400 文案双语化「postgres 或 mysql」；:410 target tidb 守卫保留。
- mysql/source.go `Capabilities.Compare=true`（MS-01 单一真值位）；A1 matrix+Capable 单真锚随翻。
- 前端：MS-01 能力位驱动解灰自动生效（零逻辑改动）；:775 alert 文案去「等待 MS-08」；前端 hash **index-D1VYsRW1.js / index-TbKke_6_.css**。

## A3 账

- ①新增 2 行 justify（routing 非 gating）：config.go DSNByType 分派 1→2、validator.go 装配分派 0→1（compare 吸收属②净回）。
- ② compare.go 3→1（:410 target 留）。**终账 11 持平**（11+2−2）；MS-12 清零目标不变。

## 门禁 A 案局限记档

- TiDB 充 MySQL 源覆盖：`?` 占位符/information_schema/statistics 键探测/DATA_TYPE 白名单/值形态（文本协议）。
- 覆盖不到（原生 MySQL only）：zero-date、unsigned 边缘、真 MySQL CHAR 截尾引擎差异——白盒锚+MS-09+ 观察池，B 案（真 MySQL fixture）候刘源令。

## 分笔哈希账

| commit | 内容 | 件哈希 |
| --- | --- | --- |
| 29b4a78 | ①芯（8 文件 +507/−35） | 见 config 锚笔实测 |
| bd788ce | ②闸（6 文件 +83/−28，含前端产物） | 同上（构建含新 dist） |
| （docs） | ③本文件 | doc-only 不动铁律 |
| （config） | ④ scripts/pg-gate.config.json 锚 bump | 逐笔实测贴 |
