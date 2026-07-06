import argparse
import os
import signal
import sys
import time


def parse_args():
    parser = argparse.ArgumentParser(description="Hold one AMD/ROCm GPU with PyTorch.")
    parser.add_argument("--gpu", type=int, default=0, help="Host GPU index to expose.")
    parser.add_argument("--mem-mb", type=int, default=1024, help="Approximate VRAM to allocate.")
    parser.add_argument("--duration", type=int, default=0, help="Seconds to run; 0 means forever.")
    parser.add_argument("--matrix", type=int, default=2048, help="Square matrix size for compute loop.")
    return parser.parse_args()


def main():
    args = parse_args()

    # Must be set before importing torch. PyTorch on ROCm still uses torch.cuda.
    gpu = str(args.gpu)
    os.environ["HIP_VISIBLE_DEVICES"] = gpu

    try:
        import torch
    except ImportError:
        print("PyTorch is required: pip install torch", file=sys.stderr)
        return 2

    if not torch.cuda.is_available():
        print("No ROCm/CUDA GPU is visible to PyTorch.", file=sys.stderr)
        return 1

    stop = False

    def handle_signal(_signum, _frame):
        nonlocal stop
        stop = True

    signal.signal(signal.SIGINT, handle_signal)
    signal.signal(signal.SIGTERM, handle_signal)

    device = torch.device("cuda:0")
    print(f"holding host gpu={args.gpu} as torch device={device}")

    tensors = []
    chunk_mb = 256
    element_size = torch.empty((), dtype=torch.float32, device=device).element_size()
    elems_per_chunk = chunk_mb * 1024 * 1024 // element_size
    chunks = max(1, args.mem_mb // chunk_mb)

    try:
        for _ in range(chunks):
            tensors.append(torch.empty(elems_per_chunk, dtype=torch.float32, device=device))
        torch.cuda.synchronize()
        print(f"allocated ~{chunks * chunk_mb} MiB on gpu {args.gpu}")
    except RuntimeError as err:
        print(f"allocation failed after ~{len(tensors) * chunk_mb} MiB: {err}", file=sys.stderr)
        return 1

    a = torch.randn((args.matrix, args.matrix), device=device)
    b = torch.randn((args.matrix, args.matrix), device=device)
    deadline = None if args.duration <= 0 else time.monotonic() + args.duration

    iterations = 0
    while not stop and (deadline is None or time.monotonic() < deadline):
        c = a @ b
        a = c.relu()
        iterations += 1
        if iterations % 10 == 0:
            torch.cuda.synchronize()
            print(f"still holding gpu {args.gpu}; iterations={iterations}", flush=True)

    torch.cuda.synchronize()
    print(f"released gpu {args.gpu}; iterations={iterations}")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
