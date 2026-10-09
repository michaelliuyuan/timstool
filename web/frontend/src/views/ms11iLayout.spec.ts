// MS-11i pen1 FE anchors (leader order seq230, 刘源 approval seq229):
// ① responsive tables — every data table wraps in .tims-table-scroll (shared
//    rule in design-tokens.css, single source; pages must not copy it locally)
//    and low-priority columns are pruned at <=900px via class-name="hide-sm";
// ② TaskList empty face — table hidden when empty so el-empty + CTA is the
//    single empty message (was stacked with the built-in 暂无数据 row);
// ③ row-action buttons unified to link style across History/Compare lists;
// ④ wizard test-connection buttons demoted to plain — one primary per step
//    (下一步), no more stacked dual primaries (G4).
// Assertion shape = spec exact match (no page blind-scan).
import { describe, expect, it } from 'vitest'
import { readFileSync } from 'node:fs'
import { resolve } from 'node:path'

const src = (f: string) => readFileSync(resolve(__dirname, f), 'utf8')

describe('MS-11i pen1: responsive tables + empty/action unification', () => {
  it('shared single-source responsive table utilities live in design-tokens.css only', () => {
    const css = src('../styles/design-tokens.css')
    expect(css).toContain('.tims-table-scroll {')
    expect(css).toContain('.tims-table-scroll .hide-sm')
    // The three pages must NOT carry local copies of the rule.
    for (const f of ['TaskListView.vue', 'HistoryView.vue', 'CompareView.vue']) {
      expect(src(f)).not.toContain('.tims-table-scroll {')
    }
  })

  it('TaskList: scroll wrap + hide-sm pruning + single empty face with CTA', () => {
    const s = src('TaskListView.vue')
    expect(s).toContain('<div v-if="tasks.length > 0" class="tims-table-scroll">')
    expect(s).toContain('label="表进度" width="120" class-name="hide-sm"')
    expect(s).toContain('label="行数" width="140" class-name="hide-sm"')
    expect(s).toContain('label="创建时间" width="180" class-name="hide-sm"')
    // Empty CTA stays; the built-in empty row is gone with the v-if table.
    expect(s).toContain('创建第一个迁移任务')
    expect(s).not.toContain('empty-text')
  })

  it('History: scroll wrap + hide-sm pruning + link-style row actions', () => {
    const s = src('HistoryView.vue')
    expect(s).toContain('<div v-if="tasks.length > 0" class="tims-table-scroll">')
    expect(s).toContain('label="行数" width="140" class-name="hide-sm"')
    expect(s).toContain('label="创建时间" width="180" class-name="hide-sm"')
    // Unified with CompareView: link buttons, not bordered/plain pairs.
    expect(s).toContain('size="small" link type="primary"')
    expect(s).toContain('size="small" link type="danger"')
    expect(s).not.toContain('type="danger" plain @click="deleteTask')
  })

  it('Compare: both tables scroll-wrapped + history/result columns pruned', () => {
    const s = src('CompareView.vue')
    expect(s.split('class="tims-table-scroll"').length - 1).toBe(2)
    expect(s).toContain('prop="id" label="ID" width="90" class-name="hide-sm"')
    expect(s).toContain('label="表进度" width="120" class-name="hide-sm"')
    expect(s).toContain('label="创建时间" width="170" class-name="hide-sm"')
    expect(s).toContain('prop="duration" label="耗时" width="90" class-name="hide-sm"')
  })

  it('Wizard: test-connection buttons are plain, one primary per step (G4)', () => {
    const s = src('WizardView.vue')
    expect(s).toContain('<el-button :loading="testingSource"')
    expect(s).toContain('<el-button :loading="testingTarget"')
    expect(s).not.toContain('type="primary" :loading="testingSource"')
    expect(s).not.toContain('type="primary" :loading="testingTarget"')
    // The step primary remains exactly the 下一步 button.
    expect(s).toContain('type="primary" @click="nextStep"')
  })
})
