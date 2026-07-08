#!/usr/bin/env python3
import argparse
import json
import os
import subprocess
import sys
import tempfile
import textwrap
import time
from pathlib import Path


def parse_args():
    parser = argparse.ArgumentParser(description="Run light Rocguard integration tests on a bare-metal AMD GPU host.")
    parser.add_argument("--rocguard", default="./rocguard", help="Path to rocguard binary.")
    parser.add_argument("--gpus", required=True, help="Comma-separated host GPU list, for example: 2 or 2,3.")
    parser.add_argument("--key", default=os.environ.get("KEY", ""), help="Rocguard token; defaults to KEY env.")
    parser.add_argument("--root-key", default=os.environ.get("ROOT_KEY", ""), help="Rocguard root key; defaults to ROOT_KEY env.")
    parser.add_argument("--duration", type=int, default=8, help="Seconds for each GPU workload.")
    parser.add_argument("--mem-mb", type=int, default=64, help="Approximate VRAM per GPU per process.")
    parser.add_argument("--matrix", type=int, default=128, help="Small matrix size for low-utilization tests.")
    parser.add_argument("--sleep", type=float, default=0.2, help="Sleep between compute iterations.")
    parser.add_argument("--children", type=int, default=2, help="Child process count for cgroup test.")
    parser.add_argument("--bypass-ttl", default="30m", help="TTL for auto-bypass rules.")
    parser.add_argument("--no-auto-bypass", action="store_true", help="Do not bypass pre-existing GPU PIDs.")
    parser.add_argument("--docker-image", default="", help="Optional ROCm/PyTorch image for Docker test.")
    parser.add_argument("--docker-sudo", action="store_true", help="Run docker commands through sudo.")
    parser.add_argument("--k8s-namespace", default="", help="Optional namespace for K8s test.")
    parser.add_argument("--k8s-image", default="", help="Optional ROCm/PyTorch image for K8s pod test.")
    parser.add_argument("--k8s-gpu-resource", default="amd.com/gpu", help="K8s GPU resource name.")
    parser.add_argument("--k8s-create-namespace", action="store_true", help="Create namespace if missing.")
    args = parser.parse_args()
    args.gpu_list = parse_gpu_list(args.gpus)
    if not args.key:
        raise SystemExit("KEY token is required. Pass --key or export KEY=rg_...")
    needs_root_key = not args.no_auto_bypass or bool(args.docker_image) or bool(args.k8s_namespace and args.k8s_image)
    if needs_root_key and not args.root_key:
        raise SystemExit("ROOT_KEY is required for auto-bypass and cleanup. Pass --root-key or export ROOT_KEY=rk_...")
    return args


def parse_gpu_list(value):
    gpus = []
    seen = set()
    for part in value.split(","):
        part = part.strip()
        if not part:
            continue
        gpu = int(part)
        if gpu < 0:
            raise SystemExit(f"invalid gpu index: {gpu}")
        if gpu in seen:
            raise SystemExit(f"duplicate gpu index: {gpu}")
        seen.add(gpu)
        gpus.append(gpu)
    if not gpus:
        raise SystemExit("at least one GPU is required")
    return gpus


def main():
    args = parse_args()
    root = Path(__file__).resolve().parents[1]
    rocguard = str((root / args.rocguard).resolve()) if not os.path.isabs(args.rocguard) else args.rocguard
    env = os.environ.copy()
    env["KEY"] = args.key
    if args.root_key:
        env["ROOT_KEY"] = args.root_key

    print(f"rocguard={rocguard}")
    print(f"gpus={args.gpus}, duration={args.duration}s, mem={args.mem_mb}MiB, matrix={args.matrix}, sleep={args.sleep}s")

    if not args.no_auto_bypass:
        auto_bypass_existing_processes(args, rocguard, env)

    test_multigpu(args, root, rocguard, env)
    test_child_processes(args, root, rocguard, env)

    if args.docker_image:
        test_docker(args, root, rocguard, env)
    else:
        print("[skip] docker: pass --docker-image <rocm-pytorch-image> to run")

    if args.k8s_namespace and args.k8s_image:
        test_k8s(args, rocguard, env)
    else:
        print("[skip] k8s: pass --k8s-namespace <ns> --k8s-image <image> to run")

    print("integration tests completed")
    return 0


def run(cmd, *, env=None, capture=False, check=True):
    printable = " ".join(str(part) for part in cmd)
    print(f"+ {printable}", flush=True)
    if capture:
        result = subprocess.run(cmd, env=env, text=True, stdout=subprocess.PIPE, stderr=subprocess.STDOUT)
        print(result.stdout, end="")
    else:
        result = subprocess.run(cmd, env=env)
    if check and result.returncode != 0:
        raise SystemExit(f"command failed ({result.returncode}): {printable}")
    return result


def run_json(cmd, *, env=None):
    result = run(cmd, env=env, capture=True)
    start = result.stdout.find("{")
    if start < 0:
        raise SystemExit(f"expected JSON object in output from: {' '.join(cmd)}")
    return json.loads(result.stdout[start:])


def auto_bypass_existing_processes(args, rocguard, env):
    print("[setup] auto-bypass existing GPU processes on selected GPUs")
    processes = amd_smi_processes(set(args.gpu_list))
    if not processes:
        print("[setup] no pre-existing GPU PIDs found on selected GPUs")
        return
    seen = set()
    for process in processes:
        pid = process["pid"]
        if pid in seen:
            continue
        seen.add(pid)
        if not Path(f"/proc/{pid}").exists():
            print(f"[setup] skip stale pid={pid} gpu={process['gpu']}")
            continue
        run(
            admin_cmd(args, rocguard)
            + ["bypass", "add", "--pid", str(pid), "--ttl", args.bypass_ttl, "--reason", "rocguard-integration-preexisting"],
            env=env,
            check=True,
        )


def amd_smi_processes(selected_gpus):
    result = run(["amd-smi", "process", "--json"], capture=True, check=False)
    if result.returncode != 0:
        print("[warn] amd-smi process --json failed; skipping auto-bypass")
        return []
    data = trim_json_array(result.stdout)
    if not data:
        print("[warn] amd-smi output did not contain JSON array; skipping auto-bypass")
        return []
    raw = json.loads(data)
    out = []
    for gpu_entry in raw:
        gpu = int(gpu_entry.get("gpu", -1))
        if gpu not in selected_gpus:
            continue
        for process in gpu_entry.get("process_list", []):
            info = process.get("process_info", {})
            pid = info.get("pid")
            if isinstance(pid, int) and pid > 0:
                out.append({"gpu": gpu, "pid": pid})
    return out


def trim_json_array(output):
    start = output.find("[")
    end = output.rfind("]")
    if start < 0 or end < start:
        return ""
    return output[start : end + 1]


def hold_args(args, gpu_value):
    return [
        "--gpus",
        str(gpu_value),
        "--mem-mb",
        str(args.mem_mb),
        "--duration",
        str(args.duration),
        "--matrix",
        str(args.matrix),
        "--sleep",
        str(args.sleep),
    ]


def test_multigpu(args, root, rocguard, env):
    print("[test] multi-gpu holder")
    gpu_csv = ",".join(str(gpu) for gpu in args.gpu_list)
    run(
        [
            rocguard,
            "run",
            "--",
            sys.executable,
            str(root / "scripts" / "hold_gpu.py"),
        ]
        + hold_args(args, gpu_csv),
        env=env,
    )


def test_child_processes(args, root, rocguard, env):
    print("[test] child processes stay authorized in rocguard cgroup")
    run(
        [
            rocguard,
            "run",
            "--",
            sys.executable,
            str(root / "scripts" / "spawn_gpu_children.py"),
            "--gpus",
            str(args.gpu_list[0]),
            "--children",
            str(args.children),
            "--mem-mb",
            str(args.mem_mb),
            "--duration",
            str(args.duration),
            "--matrix",
            str(args.matrix),
            "--sleep",
            str(args.sleep),
        ],
        env=env,
    )


def docker_cmd(args):
    prefix = ["sudo"] if args.docker_sudo else []
    return prefix + ["docker"]


def test_docker(args, root, rocguard, env):
    print("[test] docker container authorization")
    name = f"rocguard-it-{os.getpid()}"
    mount = f"{root}:/work"
    try:
        run(docker_cmd(args) + ["rm", "-f", name], check=False)
        run(
            docker_cmd(args)
            + [
                "run",
                "-d",
                "--name",
                name,
                "--device=/dev/kfd",
                "--device=/dev/dri",
                "--group-add",
                "video",
                "--group-add",
                "render",
                "--ipc=host",
                "-v",
                mount,
                "-w",
                "/work",
                args.docker_image,
                "sleep",
                "infinity",
            ]
        )
        allow = run_json([rocguard, "allow", "docker", "--container", name], env=env)
        run(docker_cmd(args) + ["exec", name, "python3", "scripts/hold_gpu.py"] + hold_args(args, args.gpu_list[0]))
        if allow.get("authorization_id"):
            run([rocguard, "revoke", allow["authorization_id"]], env=env, check=False)
    finally:
        run(docker_cmd(args) + ["rm", "-f", name], check=False)


def test_k8s(args, rocguard, env):
    print("[test] k8s namespace authorization")
    namespace = args.k8s_namespace
    pod = f"rocguard-it-{os.getpid()}"
    if args.k8s_create_namespace:
        run(["kubectl", "create", "namespace", namespace], check=False)
    allow = run_json([rocguard, "allow", "k8s", "--namespace", namespace], env=env)
    manifest = k8s_manifest(args, namespace, pod)
    try:
        with tempfile.NamedTemporaryFile("w", suffix=".yaml", delete=False) as tmp:
            tmp.write(manifest)
            tmp_path = tmp.name
        run(["kubectl", "apply", "-f", tmp_path])
        run(["kubectl", "wait", "--for=condition=Ready", f"pod/{pod}", "-n", namespace, "--timeout=120s"], check=False)
        run(["kubectl", "wait", "--for=condition=Succeeded", f"pod/{pod}", "-n", namespace, "--timeout=300s"])
        run(["kubectl", "logs", pod, "-n", namespace], check=False)
    finally:
        if "tmp_path" in locals():
            Path(tmp_path).unlink(missing_ok=True)
        run(["kubectl", "delete", "pod", pod, "-n", namespace, "--ignore-not-found=true"], check=False)
        if allow.get("authorization_id"):
            run([rocguard, "revoke", allow["authorization_id"]], env=env, check=False)


def k8s_manifest(args, namespace, pod):
    code = textwrap.dedent(
        f"""
        import os, time
        os.environ["HIP_VISIBLE_DEVICES"] = "0"
        import torch
        assert torch.cuda.is_available(), "gpu is not visible"
        device = torch.device("cuda:0")
        tensors = [torch.empty({args.mem_mb} * 1024 * 1024 // 4, dtype=torch.float32, device=device)]
        a = torch.randn(({args.matrix}, {args.matrix}), device=device)
        b = torch.randn(({args.matrix}, {args.matrix}), device=device)
        deadline = time.monotonic() + {args.duration}
        while time.monotonic() < deadline:
            a = (a @ b).relu()
            time.sleep({args.sleep})
        torch.cuda.synchronize(device)
        print("k8s gpu smoke complete")
        """
    ).strip()
    return f"""apiVersion: v1
kind: Pod
metadata:
  name: {pod}
  namespace: {namespace}
spec:
  restartPolicy: Never
  containers:
  - name: hold
    image: {args.k8s_image}
    command: ["python3", "-c", {json.dumps(code)}]
    resources:
      limits:
        {args.k8s_gpu_resource}: "1"
"""


if __name__ == "__main__":
    raise SystemExit(main())
