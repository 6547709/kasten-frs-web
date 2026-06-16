# kasten-frs-web 部署 / 测试经验（2026-06-16）

## 1. OCP `restricted-v2` SCC + 命名空间 UID range 冲突

- OCP `restricted-v2` SCC 有 `runAsUser.type=MustRunAsRange`，要求
  显式 runAsUser 落在命名空间分配的 UID range 内。
- 仓库默认命名空间 `kasten-io` 在我们这个集群里 range 是
  `1001100000/10000`。
- deploy/20-deployment.yaml 早期 pin 了 `runAsUser: 1001`，落在
  range 之外，被准入拒绝，pod 起不来。修法：去掉 runAsUser pin（让
  SCC 从 range 里挑），加 `fsGroup: 1001100000` 让 emptyDir 可写。
  详见 commit `2aed67f`。

## 2. K10 default-deny NetworkPolicy（selector={}）

- K10 控制平面装好后，会自动注入一个 selector={} 的 NetworkPolicy，
  policyTypes=[Ingress]，**默认**拒绝所有进入 kasten-io pod 的流量
  （包括我们自己部署的 helper）。
- 即使我们的 deploy/50-networkpolicy.yaml 显式写了
  port-8080 ingress from openshift-ingress 也不行 —— 因为 OVN-K
  处理 K10 注入的 default-deny 时把所有后续规则都吞了。
- 解法：用 Kustomize 在 overlay 里 `op: replace` 把整个 `spec/egress`
  替换成 `[{to: [{}]}]`（放行所有出站），`spec/ingress` 替换为允许
  openshift-ingress 命名空间到 8080 的规则。详见
  `scripts/deploy-test.sh` 注释 + commit `dcee3e1`。
- 旁路：K10 pod 自己也跑在 kasten-io，它们的 netpol 状态是
  "Ingress-only"（无 egress 规则）—— 我们也照此办理。

## 3. FRS SFTP pod 的自动 NetworkPolicy

- K10 datamover 每次创建 FRS 时会同步生成一个 NetworkPolicy
  `frs-XXXXX`，只允许**源 namespace=default** 的流量到 FRS pod 的
  2222。helper pod 在 kasten-io，被这个 netpol 拒绝。
- 解法：在 overlay 加一个 ingress 规则：from helper podSelector 到
  FRS podSelector 2222。K10 的 netpol 不会限制 ingress 源集合的合并
  —— 多个 netpol 的规则是并集。
- 详见 `scripts/deploy-test.sh` 的 frs-allow.yaml 段。

## 4. GHCR 匿名拉镜像

- `kubectl get pods` 等到 ImagePullBackOff，根因往往是 ghcr.io
  匿名访问要走 OCI Accept 头：
  ```
  Accept: application/vnd.oci.image.index.v1+json
  ```
  + 先取 bearer token（`/v2/.../manifests/...` 401 响应里
  `WWW-Authenticate: Bearer realm=...`），再用 token 调 manifest。
- 详见 `scripts/deploy-test.sh` preflight 段。

## 5. FRS 状态机

```
Starting → Processing → Ready
                    ↘  Failed
```

- `Ready` 之前 `status.transports.sftp.{serviceName, portNumber,
  hostKeySignature}` 都不可读（空字符串或缺失）。
- helper 必须在 `Ready` 之后才能 SFTP dial；早期 dial 会拿到
  `connection refused` 或 `ssh: handshake failed`。
- K10 helm chart 的 `frs.sessionExpiryTimeInMinutes`（默认 60min）
  是 FRS 存活上限；超时会自动清理 pod + service + netpol。
- helper 端在 `WaitForReady` 之上加**内存 watch map**（spec §4.4）
  是为了避免在 HTTP handler 里同步等 30s+ 触发 HAProxy 默认 30s
  request timeout。

## 6. e2e 网络验证三件套

helper pod 起来后用 `oc exec` 跑这三个就能定位大多数网络问题：

```
# DNS
oc -n kasten-io exec $POD -- nslookup kubernetes.default
# K8s API
oc -n kasten-io exec $POD -- curl -sk https://kubernetes.default.svc/api
# FRS SFTP（替换 frs-xxx）
oc -n kasten-io exec $POD -- bash -c "timeout 3 bash -c '</dev/tcp/frs-xxx.kasten-io.svc.cluster.local/2222'"
```

不用 `nslookup` + `curl` 完整包名：直接 `getent hosts
kubernetes.default` 在某些精简镜像里也行。

## 7. Route edge TLS 默认 30s/300s 响应超时

- OCP Route edge termination 默认 `haproxy.router.openshift.io/timeout=300s`
  （响应）+ 30s（请求）。
- 10G+ 文件下载通常超过 300s，需要在 Route 上加 annotation：
  ```
  haproxy.router.openshift.io/timeout: 86400s
  ```
  body 缓冲 `buffer-size` 默认 0=无限制，不用调。
- 另：SFTP 池 idle TTL `HELPER_SFTP_TTL` 默认 30min；超过 30min
  的下载 helper 端 SFTP 会断，需要重连。10G 量级建议设
  `HELPER_SFTP_TTL=2h`。
