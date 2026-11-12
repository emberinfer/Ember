# Ember Engine SPI Conformance Kit 0.2.0

This archive is standalone. Read `SPEC.md`, implement an adapter using
`ember_spi.py`, then run it against the released real node host:

```sh
EMBER_NODE=/path/to/release/bin/ember-node ./run-conformance.sh
```

`examples/toy_adapter.py` and `toy_http_engine.py` are the minimal walkthrough.
They use only this kit and the Python standard library. A PASS proves interface
and evidence-shape conformance, not model correctness or production eligibility.
