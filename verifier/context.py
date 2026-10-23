"""Shared state of one verify run: paths, parameters and the simulated runs."""

import os
import shutil
import subprocess
import time

from verifier import formats

PARAMS_KEYS = (
    "campaign_messages", "campaign_kills", "campaign_kills_post_250",
    "retry_policy", "campaign_retry_policy", "isolation_margin",
    "tenant_bounce_threshold", "tenant_window", "tenant_pause_bound_s",
    "warmup_plan", "warmup_share_tolerance", "memory_limit_bytes",
)
POLICY_KEYS = ("initial_s", "factor", "max_s", "lifetime_s")
CAMPAIGN_SEED = 1


def load_params(path):
    """lab/params.json (contract 5): exactly the keys of section 5."""
    params = formats.load_json(path)
    if not isinstance(params, dict):
        raise formats.FormatError(f"{path}: not an object")
    unknown = sorted(set(params) - set(PARAMS_KEYS))
    missing = sorted(set(PARAMS_KEYS) - set(params))
    if unknown or missing:
        raise formats.FormatError(f"{path}: unknown keys {unknown}, missing keys {missing}")
    for key in ("retry_policy", "campaign_retry_policy"):
        if sorted(params[key]) != sorted(POLICY_KEYS):
            raise formats.FormatError(f"{path}: {key} needs exactly {list(POLICY_KEYS)}")
    return params


class Context:
    def __init__(self, root, out, bin_dir=None, params=None):
        self.root = os.path.abspath(root)
        self.out = os.path.abspath(os.path.join(self.root, out))
        self.bin = os.path.abspath(bin_dir or os.path.join(self.root, "bin"))
        self.params = params if params is not None else load_params(
            os.path.join(self.root, "lab", "params.json"))
        self.started = time.monotonic()
        self._campaign = None

    def path(self, *parts):
        return os.path.join(self.root, *parts)

    def rel(self, path):
        """Path relative to the repository root, as the report shows it."""
        return os.path.relpath(path, self.root)

    def run_dir(self, name):
        return os.path.join(self.out, "runs", name)

    def fresh_dir(self, name):
        """An empty out/runs/<name>; only that directory under out/runs is removed."""
        path = self.run_dir(name)
        if os.path.lexists(path):
            shutil.rmtree(path)
        os.makedirs(path)
        return path

    def mtadst(self, *args):
        """Run bin/mtadst; the engine is only ever a subprocess of the verifier."""
        cmd = [os.path.join(self.bin, "mtadst"), *args]
        res = subprocess.run(cmd, cwd=self.root, capture_output=True, text=True)
        if res.returncode != 0:
            raise RuntimeError(f"{' '.join(cmd)}: exit {res.returncode}: {res.stderr.strip()}")
        return res.stdout

    def simulate(self, scenario, seed, out_dir, variant=None):
        """mtadst run of lab/scenarios/<scenario>.json, or of one of its variants,
        into an empty out_dir."""
        if os.path.lexists(out_dir):
            shutil.rmtree(out_dir)
        os.makedirs(os.path.dirname(out_dir), exist_ok=True)
        extra = ["--variant", variant] if variant else []
        self.mtadst("run", "--scenario", self.path("lab", "scenarios", scenario + ".json"), *extra,
                    "--seed", str(seed), "--out", out_dir, "--root", self.root,
                    "--research", os.path.join("vault", "RESEARCH.md"))
        return out_dir

    def lab_dir(self, seed):
        """out/runs/lab-<seed>: the runs receiver_limits decides on, simulated and real."""
        return self.run_dir(f"lab-{seed}")

    def campaign(self, seed=CAMPAIGN_SEED):
        """The real kill campaign (3.12) in out/runs/lab-<seed>/campaign, run once per
        verify and shared by the properties that decide on it."""
        if self._campaign is None:
            from verifier import campaign  # the lab needs the namespace; load it on use

            run_dir = os.path.join(self.lab_dir(seed), "campaign")
            if os.path.lexists(run_dir):
                shutil.rmtree(run_dir)
            os.makedirs(run_dir)
            self._campaign = {"seed": seed, "dir": run_dir,
                              "summary": campaign.run(self, seed, run_dir)}
        return self._campaign
