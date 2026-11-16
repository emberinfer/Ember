"""Registration availability is independent of engine adapter capabilities."""


def opt_outs(args, capabilities=None):
    no_audit = bool(getattr(args, 'no_audit', False))
    no_exec = bool(getattr(args, 'no_exec', False))
    if getattr(args, 'audit_role', False):
        if no_audit:
            raise ValueError('--audit-role conflicts with --no-audit')
        # Historically join appended auditor to the engine's existing roles.
        # Model/shard engines therefore offered BOTH roles, not audit-only.
        roles = getattr(capabilities, 'roles', ()) if capabilities is not None else ()
        no_exec = no_exec or not any(r in roles for r in
            ('model', 'executor', 'shard-stage', 'moe-attention', 'moe-expert'))
    return {'no_audit': no_audit, 'no_exec': no_exec}
