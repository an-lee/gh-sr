# Local Actions Cache

gh-sr 可以在每台 Linux 主机上部署一个本地 Actions cache 服务器,使 `actions/cache`、`actions/cache/restore|save` 以及 gh-aw 的 `cache-memory`、usage cache 等全部落在本机,不再在每次任务开始时从 GitHub 下载缓存。

> 本页是骨架文档,随 Phase B(cache 集成)实施补全。

## 方案

采用 [falcondev-oss/github-actions-cache-server](https://github.com/falcondev-oss/github-actions-cache-server) —— Actions cache **v2 协议**的 drop-in 替代,官方 `actions/cache` 无需改动工作流。

- **拓扑**:每台 Linux 主机一个 `gh-sr-cache` 容器,该机上所有 container 模式 runner 共享。
- **接线**:runner 容器注入 `CUSTOM_ACTIONS_RESULTS_URL`(指向本机 cache server)。runner 二进制必须来自 [falcondev fork 镜像](https://github.com/falcondev-oss/github-actions-runner)(gh-sr 的容器镜像已以其为 base),否则 runner 会用 job message 里的 `ACTIONS_RESULTS_URL` 覆盖环境变量,缓存静默回源 GitHub。
- **artifacts 不受影响**:`upload-artifact`/`download-artifact` 等非 cache 请求由 cache server 透传回 GitHub(`DEFAULT_ACTIONS_RESULTS_URL`),跨主机 safe-outputs 仍然正常。
- **安全**:cache server 绑定 docker0 网关 IP(仅本机容器可达),并校验 runner 的 OIDC token。

## CLI(规划中)

```
gh sr cache status            # 各主机 cache 状态、存储占用
gh sr cache deploy            # 显式部署/升级
gh sr cache prune             # 触发管理 API 清理
gh sr cache remove [--purge-data]   # 卸载(--purge-data 才删数据)
```

`cache.enabled` 默认开启;`gh sr setup`/`gh sr up` 会自动确保 cache server 存在。
