"""Operator epoch audit report from chain state and optional measured work costs.

python -m auditor.pnl --state state.json --costs costs.json --output pnl.json
Costs: [{hold_id, auditor, cost_subunits}]. Absence means unknown P&L, not zero.
"""
import argparse
import json
from pathlib import Path


def epoch_report(state, costs=()):
    lookup = {}
    for cost in costs:
        key = (cost['hold_id'], cost['auditor'])
        if key in lookup or not isinstance(cost['cost_subunits'], int) or cost['cost_subunits'] < 0:
            raise ValueError('duplicate or invalid cost')
        lookup[key] = cost['cost_subunits']
    rows = {}
    for hold_id, hold in sorted(state.get('holds', {}).items()):
        receipts = iter(hold.get('audit_receipts') or [])
        for work in hold.get('audit_works') or []:
            key = (work['epoch'], work['auditor'])
            row = rows.setdefault(key, dict(epoch=key[0], auditor=key[1], assigned=0,
                replayed=0, comp_earned=0, share_shortfalls=0, cost_subunits=0, missing_costs=0))
            row['assigned'] += 1
            if not work['replayed']:
                continue
            row['replayed'] += 1
            # Unsettled work earns zero so far; a future report may include payment.
            receipt = next(receipts, None)
            if receipt:
                comp = receipt['audit_comp']
                if comp['auditor'] != work['auditor']:
                    raise ValueError('receipt/assignment order mismatch')
                row['comp_earned'] += int(comp.get('comp_subunits', 0))
                row['share_shortfalls'] += int(receipt.get('audit_shortfall_subunits', 0))
            cost = lookup.get((hold_id, work['auditor']))
            if cost is None:
                row['missing_costs'] += 1
            else:
                row['cost_subunits'] += cost
    epochs = set(state.get('report_epochs', [])) | {key[0] for key in rows}
    if not epochs:
        blocks = state.get('parameters', {}).get('audit_work', {}).get('epoch_blocks', 1000)
        epochs.add(state.get('height', 0)//blocks)
    for epoch in epochs:
        for auditor, enabled in state.get('auditors', {}).items():
            if enabled:
                rows.setdefault((epoch, auditor), dict(epoch=epoch, auditor=auditor, assigned=0,
                    replayed=0, comp_earned=0, share_shortfalls=0, cost_subunits=0, missing_costs=0))
    result = []
    for key in sorted(rows):
        row = rows[key]
        row['pnl_subunits'] = None if row['missing_costs'] else row['comp_earned']-row['cost_subunits']
        result.append(row)
    return dict(schema='auditor-epoch-pnl-v1', context=state.get('report_context', 'chain state'), rows=result)


def main():
    p = argparse.ArgumentParser(description=__doc__)
    p.add_argument('--state', required=True)
    p.add_argument('--costs')
    p.add_argument('--output', required=True)
    a = p.parse_args()
    costs = json.loads(Path(a.costs).read_text()) if a.costs else []
    result = epoch_report(json.loads(Path(a.state).read_text()), costs)
    Path(a.output).write_text(json.dumps(result, indent=2)+'\n')


if __name__ == '__main__':
    main()
