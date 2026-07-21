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
    parser = argparse.ArgumentParser(description="Run light Gpuardian integration tests on a bare-metal AMD GPU host.")
    parser.add_argument("--gpuardian", default="./gpuardian", help="Path to gpuardian binary.")
    parser.add_argument("--gpus", required=True, help="Comma-separated host GPU list, for example: 2 or 2,3.")
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
    args.key = os.environ.get("KEY", "")
    args.root_key = os.environ.get("ROOT_KEY", "")
    args.gpu_list = parse_gpu_list(args.gpus)
    if args.duration <= 0:
        raise SystemExit("--duration must be positive")
    if args.mem_mb <= 0 or args.matrix <= 0:
        raise SystemExit("--mem-mb and --matrix must be positive")
    if args.sleep < 0:
        raise SystemExit("--sleep must be >= 0")
    if args.children < 0 or args.children > 64:
        raise SystemExit("--children must be between 0 and 64")
    if not args.key:
        raise SystemExit("KEY token is required; export KEY=rg_... (secrets are not accepted in command-line arguments)")
    needs_root_key = not args.no_auto_bypass or bool(args.docker_image) or bool(args.k8s_namespace and args.k8s_image)
    if needs_root_key and not args.root_key:
        raise SystemExit("ROOT_KEY is required for auto-bypass and cleanup; export ROOT_KEY=rk_...")
    return args


def parse_gpu_list(value):
    gpus = []
    seen = set()
    for part in value.split(","):
        part = part.strip()
        if not part:
            continue
        try:
            gpu = int(part)
        except ValueError:
            raise SystemExit(f"invalid gpu index: {part!r}") from None
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
    gpuardian = str((root / args.gpuardian).resolve()) if not os.path.isabs(args.gpuardian) else args.gpuardian
    clean_env = os.environ.copy()
    for name in ("KEY", "ROOT_KEY", "GPUARDIAN_WEB_PASSWORD"):
        clean_env.pop(name, None)
    token_env = clean_env.copy()
    token_env["KEY"] = args.key
    root_env = clean_env.copy()
    if args.root_key:
        root_env["ROOT_KEY"] = args.root_key

    print(f"gpuardian={gpuardian}")
    print(f"gpus={args.gpus}, duration={args.duration}s, mem={args.mem_mb}MiB, matrix={args.matrix}, sleep={args.sleep}s")

    bypass_ids = []
    try:
        if not args.no_auto_bypass:
            auto_bypass_existing_processes(args, gpuardian, clean_env, root_env, bypass_ids)

        test_multigpu(args, root, gpuardian, token_env)
        test_child_processes(args, root, gpuardian, token_env)

        if args.docker_image:
            test_docker(args, root, gpuardian, clean_env, token_env, root_env)
        else:
            print("[skip] docker: pass --docker-image <rocm-pytorch-image> to run")

        if args.k8s_namespace and args.k8s_image:
            test_k8s(args, gpuardian, clean_env, token_env, root_env)
        else:
            print("[skip] k8s: pass --k8s-namespace <ns> --k8s-image <image> to run")
    finally:
        for bypass_id in reversed(bypass_ids):
            run([gpuardian, "revoke", bypass_id], env=root_env, check=False)

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


def auto_bypass_existing_processes(args, gpuardian, clean_env, root_env, bypass_ids):
    print("[setup] auto-bypass existing GPU processes on selected GPUs")
    processes = amd_smi_processes(set(args.gpu_list), clean_env)
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
        bypass = run_json(
            [gpuardian, "bypass", "add", "--pid", str(pid), "--ttl", args.bypass_ttl, "--reason", "gpuardian-integration-preexisting"],
            env=root_env,
        )
        if bypass.get("id"):
            bypass_ids.append(bypass["id"])


def amd_smi_processes(selected_gpus, clean_env):
    result = run(["amd-smi", "process", "--json"], env=clean_env, capture=True, check=False)
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


def test_multigpu(args, root, gpuardian, env):
    print("[test] multi-gpu holder")
    gpu_csv = ",".join(str(gpu) for gpu in args.gpu_list)
    run(
        [
            gpuardian,
            "run",
            "--",
            sys.executable,
            str(root / "scripts" / "hold_gpu.py"),
        ]
        + hold_args(args, gpu_csv),
        env=env,
    )


def test_child_processes(args, root, gpuardian, env):
    print("[test] child processes stay authorized in gpuardian cgroup")
    run(
        [
            gpuardian,
            "run",
            "--",
            sys.executable,
            str(root / "scripts" / "hold_gpu.py"),
        ]
        + hold_args(args, args.gpu_list[0])
        + [
            "--children",
            str(args.children),
        ],
        env=env,
    )


def docker_cmd(args):
    prefix = ["sudo"] if args.docker_sudo else []
    return prefix + ["docker"]


def test_docker(args, root, gpuardian, clean_env, token_env, root_env):
    print("[test] docker container authorization")
    name = f"gpuardian-it-{os.getpid()}"
    mount = f"{root}:/work:ro"
    allow = {}
    try:
        run(docker_cmd(args) + ["rm", "-f", name], env=clean_env, check=False)
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
                "-v",
                mount,
                "-w",
                "/work",
                "--entrypoint",
                "sleep",
                args.docker_image,
                "infinity",
            ],
            env=clean_env,
        )
        allow = run_json([gpuardian, "allow", "docker", "--container", name], env=token_env)
        run(
            docker_cmd(args) + ["exec", name, "python3", "scripts/hold_gpu.py"] + hold_args(args, args.gpu_list[0]),
            env=clean_env,
        )
    finally:
        if allow.get("authorization_id"):
            run([gpuardian, "revoke", allow["authorization_id"]], env=root_env, check=False)
        run(docker_cmd(args) + ["rm", "-f", name], env=clean_env, check=False)


def test_k8s(args, gpuardian, clean_env, token_env, root_env):
    print("[test] k8s namespace authorization")
    namespace = args.k8s_namespace
    pod = f"gpuardian-it-{os.getpid()}"
    if args.k8s_create_namespace:
        run(["kubectl", "create", "namespace", namespace], env=clean_env, check=False)
    allow = {}
    try:
        allow = run_json([gpuardian, "allow", "k8s", "--namespace", namespace], env=token_env)
        manifest = k8s_manifest(args, namespace, pod)
        with tempfile.NamedTemporaryFile("w", suffix=".yaml", delete=False) as tmp:
            tmp.write(manifest)
            tmp_path = tmp.name
        run(["kubectl", "apply", "-f", tmp_path], env=clean_env)
        run(["kubectl", "wait", "--for=condition=Ready", f"pod/{pod}", "-n", namespace, "--timeout=120s"], env=clean_env, check=False)
        run(["kubectl", "wait", "--for=condition=Succeeded", f"pod/{pod}", "-n", namespace, "--timeout=300s"], env=clean_env)
        run(["kubectl", "logs", pod, "-n", namespace], env=clean_env, check=False)
    finally:
        if "tmp_path" in locals():
            Path(tmp_path).unlink(missing_ok=True)
        run(["kubectl", "delete", "pod", pod, "-n", namespace, "--ignore-not-found=true"], env=clean_env, check=False)
        if allow.get("authorization_id"):
            run([gpuardian, "revoke", allow["authorization_id"]], env=root_env, check=False)


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
    manifest = {
        "apiVersion": "v1",
        "kind": "Pod",
        "metadata": {"name": pod, "namespace": namespace},
        "spec": {
            "restartPolicy": "Never",
            "containers": [
                {
                    "name": "hold",
                    "image": args.k8s_image,
                    "command": ["python3", "-c", code],
                    "resources": {"limits": {args.k8s_gpu_resource: "1"}},
                }
            ],
        },
    }
    return json.dumps(manifest, indent=2)


if __name__ == "__main__":
    raise SystemExit(main())
