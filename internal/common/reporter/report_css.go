package reporter

// ReportCSS is the shared base stylesheet for all server-rendered HTML
// reports (migration / validation / compare / assessment). It mirrors the
// web design tokens (V2.2) so downloaded reports match the UI look:
// ink #0C1222 · workspace #F6F7FB · brand red #E13C3C · teal #0FA3A3 ·
// amber #D97E00. Reports are standalone offline HTML: token values are
// inlined (no external font files).
const ReportCSS = `* { margin: 0; padding: 0; box-sizing: border-box; }
body { font-family: 'Inter', 'HarmonyOS Sans SC', 'MiSans', 'Segoe UI', 'PingFang SC', 'Microsoft YaHei', system-ui, sans-serif; background: #F6F7FB; color: #2A3040; line-height: 1.6; -webkit-font-smoothing: antialiased; }
.container { max-width: 1200px; margin: 0 auto; padding: 24px; }
.num { font-family: 'JetBrains Mono', 'Cascadia Mono', Consolas, ui-monospace, monospace; font-weight: 500; letter-spacing: -0.5px; }
.card { background: #FFFFFF; border: 1px solid #E6E8F0; border-radius: 12px; padding: 24px; margin-bottom: 20px; box-shadow: 0 1px 2px rgba(12,18,34,0.04), 0 4px 14px rgba(12,18,34,0.06); }
.card h2 { font-size: 18px; margin-bottom: 16px; color: #0C1222; border-bottom: 1px solid #E6E8F0; padding-bottom: 8px; }
.stats { display: grid; grid-template-columns: repeat(auto-fit, minmax(150px, 1fr)); gap: 16px; margin-bottom: 12px; }
.stat { text-align: center; padding: 14px 18px; background: #FFFFFF; border: 1px solid #E6E8F0; border-radius: 12px; box-shadow: 0 1px 2px rgba(12,18,34,0.04), 0 4px 14px rgba(12,18,34,0.06); }
.stat .value { font-family: 'JetBrains Mono', 'Cascadia Mono', Consolas, ui-monospace, monospace; font-weight: 500; font-size: 26px; line-height: 1.15; color: #0C1222; }
.stat .label { font-size: 12px; color: #6B7280; margin-top: 4px; letter-spacing: 0.4px; }
table { width: 100%; border-collapse: collapse; font-size: 14px; }
th { background: #F3F4FA; color: #2A3040; padding: 10px 12px; text-align: left; font-weight: 600; border-bottom: 1px solid #E6E8F0; }
td { padding: 10px 12px; border-bottom: 1px solid #E6E8F0; }
tr:last-child td { border-bottom: none; }
tr:hover td { background: #F6F7FB; }
.badge { display: inline-block; padding: 2px 10px; border-radius: 999px; font-size: 12px; font-weight: 600; }
.badge-pass, .badge-compatible { background: rgba(15,163,163,0.10); color: #0FA3A3; }
.badge-fail, .badge-incompatible { background: rgba(225,60,60,0.09); color: #E13C3C; }
.badge-warn, .badge-manual { background: rgba(217,126,0,0.10); color: #D97E00; }
.badge-skip { background: #EEF0F5; color: #6B7280; }
.overall-pass { color: #0FA3A3; }
.overall-fail { color: #E13C3C; }
.overall-warn { color: #D97E00; }
.c-teal { color: #0FA3A3; }
.c-brand { color: #E13C3C; }
.c-amber { color: #D97E00; }
.c-muted { color: #6B7280; }
.footer { text-align: center; color: #97A0B5; font-size: 12px; margin-top: 24px; }
.summary { background: #F6F7FB; border: 1px solid #E6E8F0; padding: 16px; border-radius: 8px; margin-bottom: 16px; font-size: 14px; }
.info-row { display: flex; justify-content: space-between; padding: 8px 0; border-bottom: 1px solid #E6E8F0; }
.info-row:last-child { border-bottom: none; }
.info-label { color: #6B7280; min-width: 120px; }
.info-value { font-weight: 500; text-align: right; }
@media print { body { background: #FFFFFF; } .container { padding: 0; } .card, .stat { box-shadow: none; border: 1px solid #E6E8F0; } }
`
