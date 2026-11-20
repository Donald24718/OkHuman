## 8. 往第二台 V100 机器复制推理后端时必须补的东西

- 二进制 **RUNPATH 写死** `/usr/local/cuda-12.8/targets/x86_64-linux/lib` → 只要把**同版本**这几个 .so 放进该绝对路径即可，不需要装 CUDA 全家桶：`libcudart.so.12`(12.8.90)、`libcublas.so.12`+`libcublasLt.so.12`(12.8.5.5)、`/usr/lib/x86_64-linux-gnu/libnccl.so.2`(2.31.2)、`/usr/local/cuda/targets/x86_64-linux/lib/libOpenCL.so.1`(30KB) + `ld.so.conf.d` 一条 + `ldconfig`。判据：`ldd <二进制> | grep "not found"` 为空。
- 只搬**正在用的那个模型**（models/ 里 4 个共 45G，8081 只用 12G 的那个）。
- 改脚本三行：`D=` 路径、`CUDA_VISIBLE_DEVICES`（选另一张空卡）、`LD_LIBRARY_PATH`。
- venv 的 python 是 `home=/usr/bin` 的软链，两台同为 Mint 22.3 / glibc 2.39 / py3.12 时可直接用；不同发行版要重建 venv。
- 两个后端各占一张 16G 卡（各 ~15.4–15.9G）；**单卡就够**，不需要多卡切分（strata 配置里 `gpu=1`）。
