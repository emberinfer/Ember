"""Strict targeted RPC fixture; unexpected broad queries fail tests."""
def targeted_query(state):
    def query(_cli, _rpc, path):
        if path == '/parameters':
            return state['parameters']
        kind, ident = path.strip('/').split('/', 1)
        field = {'hold': 'holds', 'account': 'accounts', 'node': 'nodes', 'model': 'models'}[kind]
        return state[field][ident]
    return query
