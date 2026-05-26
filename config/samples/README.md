# Example policies

Ready-to-apply CRD samples. Tune the field values to your footprint.

```bash
kubectl apply -f config/samples/
```

Each sample has `humanArm: true` in the spec — the operator observes
and logs but does not escalate until you run
`kubehero cap --arm --policy <name>`.
