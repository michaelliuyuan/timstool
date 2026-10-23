// 第 7 单锚点用例：例外表单行布局（v3.19.29）
// 背景：v3.19.28 例外表每行控件总宽 886px > 弹窗内容宽 848px，折成 2-3 行（实测 103 旁路
// Playwright 三档视口 1280/1366/1920）。本单收紧为单行：
//   表名 220→170、水位列 260→200、初始水位 200→170（placeholder 同步缩短）、
//   「删除」→ ✕ 图标（悬停提示）、格式提示整行块 → 行内 ⓘ 悬停、
//   .inc-table-row nowrap + 窄屏(<900px)恢复换行兜底。
// 用例采用源码锚点方式（与 incrementalWmExceptAuto.spec.ts 同风格）：锚点漂移即失败。

import { describe, expect, it } from 'vitest'
import { readFileSync } from 'node:fs'
import { resolve } from 'node:path'

const src = readFileSync(resolve(__dirname, 'IncrementalView.vue'), 'utf8')

describe('incrementalWmRowLayout v3.19.29', () => {
  it('例外表行容器改为不换行（单行布局）', () => {
    expect(src).toContain('.inc-table-row {')
    const css = src.slice(src.indexOf('.inc-table-row {'))
    const block = css.slice(0, css.indexOf('}') + 1)
    expect(block).toContain('flex-wrap: nowrap')
    expect(block).not.toContain('flex-wrap: wrap')
  })

  it('窄屏兜底：900px 以下恢复换行', () => {
    expect(src).toContain('@media (max-width: 900px)')
    const mq = src.slice(src.indexOf('@media (max-width: 900px)'))
    const block = mq.slice(0, mq.indexOf('}', mq.indexOf('flex-wrap')) + 1)
    expect(block).toContain('flex-wrap: wrap')
  })

  it('控件宽度收紧：表名 170 / 水位列 200 / 初始水位 170', () => {
    expect(src).toContain('placeholder="搜索或手输表名" style="width: 170px;"')
    expect(src).toContain('placeholder="水位列" style="width: 200px; margin-left: 4px;"')
    expect(src).toContain('placeholder="水位列（如 update_time）" style="width: 200px; margin-left: 4px;"')
    expect(src.match(/style="width: 170px;" @change="touchRow\(i\)"/g)?.length).toBe(3)
  })

  it('初始水位 placeholder 缩短且不再出现旧长文案', () => {
    expect(src).toContain('placeholder="留空=全量补齐"')
    expect(src).toContain('placeholder="初始水位，留空全量"')
    expect(src).not.toContain('留空=从最小值全量补齐" style="width: 200px')
    expect(src).not.toContain('初始水位，留空=全量补齐" style="width: 200px')
  })

  it('删除按钮改为 ✕ 图标（悬停提示 + aria-label 可达性）', () => {
    expect(src).toContain('aria-label="删除" @click="removeRow(i)"')
    expect(src).toContain('<el-icon :size="14"><Close /></el-icon>')
    expect(src).not.toContain('@click="removeRow(i)">删除</el-button>')
  })

  it('行级格式提示改为行内 ⓘ 悬停（原整行铺底块移除）', () => {
    expect(src).toContain(':content="wmHintOf(rowWmCol(i)!.data_type)" placement="top"')
    expect(src).toContain('class="row-hint-ico"')
    expect(src).not.toContain('class="wm-hint row-hint"')
    expect(src).not.toContain('width: 100%;\n  padding-left: 2px;')
  })

  it('批量配置区提示不受本单影响（保留 wm-hint 块）', () => {
    expect(src).toContain('<div class="wm-hint">{{ wmHintOf(batchColumnType) }}</div>')
  })

  it('自动列入标签与汇总文案保持 v3.19.28 语义零改动', () => {
    expect(src).toContain('<el-tag v-if="t._autoExcept" type="info" size="small" effect="plain" style="margin-left: 4px;">自动列入</el-tag>')
    expect(src).toContain('已自动列入例外 {{ form.tables.filter(t => t._autoExcept).length }} 表（缺列 {{ autoExceptMeta.missing }} · 特殊名 {{ autoExceptMeta.badName }} · 超限 {{ autoExceptMeta.overflow }}）')
  })
})
