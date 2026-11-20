# Initial rule catalog

Rules are source-neutral. A connector only has to emit `Signal`; the evaluator
reads the same ClickHouse lookback window for every source.

| Family | Use | Example |
| --- | --- | --- |
| absence | no heartbeat or measurement in a window | `crane-7` has no `alive` for 5m |
| threshold | value crosses a bound | temperature `> 90 celsius` |
| ratio | numerator falls relative to denominator | payments / checkouts `< 20%` |
| latency | duration grows above a limit | dispatch p95 `> 30s` |
| volume drop | recent count falls below expected volume | readings `< 50%` of baseline |

Start with one or two rules for a pilot. Keep thresholds in a versioned
`RULES_FILE` and choose the minimum sample count so a quiet tenant does not
create noise. The shipped `analytical_rules.example.json` demonstrates ratio,
absence and volume-drop rules; `rules.example.json` demonstrates generic
threshold rules.

All rules are evaluated retrospectively from ClickHouse. The node does not
keep a baseline, incident episode or alert decision only in memory.
