"""Round 6: off-the-shelf Laya against the link_topics haiku verdicts.

    python measure.py pairs.jsonl out.jsonl

pairs.jsonl comes from sample.sql. Writes one row per pair with Laya's
probability, then prints agreement overall and in the confident band.
"""
import json
import sys
import time

QUESTION = {
    "same_topic": {
        "type": "noul",
        "instructions": "Are artifact A and artifact B substantively about the same exact topic?",
        "criteria": {
            "true": "the same concrete issue, incident, feature, change or request",
            "false": "only the same general area, team, system or keyword",
        },
    }
}
BAND = 0.9  # confident when p >= BAND or p <= 1 - BAND


def state(r):
    return (
        f"Artifact A ({r['a_type']}, department {r['a_dept'] or 'unknown'}):\n{r['a_summary']}\n\n"
        f"Artifact B ({r['b_type']}, department {r['b_dept'] or 'unknown'}):\n{r['b_summary']}"
    )


def report(rows):
    n = len(rows)
    agree = sum((r["p"] >= 0.5) == r["same_topic"] for r in rows)
    band = [r for r in rows if r["p"] >= BAND or r["p"] <= 1 - BAND]
    band_agree = sum((r["p"] >= 0.5) == r["same_topic"] for r in band)
    out = {
        "pairs": n,
        "agreement": round(agree / n, 4),
        "band_coverage": round(len(band) / n, 4),
        "band_agreement": round(band_agree / len(band), 4) if band else None,
    }
    for label in (True, False):
        sub = [r for r in rows if r["same_topic"] == label]
        out[f"recall_{'yes' if label else 'no'}"] = round(
            sum((r["p"] >= 0.5) == label for r in sub) / len(sub), 4)
    out["passes_rule"] = bool(band) and out["band_coverage"] >= 0.5 and out["band_agreement"] >= 0.95
    return out


def main(src, dst):
    from laya import Agent

    rows = [json.loads(l) for l in open(src)]
    agent = Agent()
    t0 = time.time()
    preds = agent.predict_batch([state(r) for r in rows], QUESTION)
    secs = time.time() - t0
    with open(dst, "w") as f:
        for r, pr in zip(rows, preds):
            r["p"] = float(pr["answers"]["same_topic"]["noul"])
            f.write(json.dumps({k: r[k] for k in ("source", "target", "same_topic", "confidence", "tag", "a_type", "b_type", "p")}) + "\n")
    res = report(rows)
    res["seconds"] = round(secs, 1)
    print(json.dumps(res, indent=2))


if __name__ == "__main__":
    if sys.argv[1:2] == ["--selftest"]:
        rs = [{"p": .95, "same_topic": True}, {"p": .05, "same_topic": False},
              {"p": .6, "same_topic": False}, {"p": .97, "same_topic": False}]
        got = report(rs)
        assert got["agreement"] == 0.5 and got["band_coverage"] == 0.75, got
        assert abs(got["band_agreement"] - 2 / 3) < 1e-3 and not got["passes_rule"], got
        print("ok")
    else:
        main(*sys.argv[1:3])
