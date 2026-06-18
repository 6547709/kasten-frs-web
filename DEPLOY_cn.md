# 部署指南(中文版)

本文档面向将 `kasten-frs-web-helper` 部署到已经安装了 Kasten K10
及 `filerecoverysessions.datamover.kio.kasten.io` CRD 的 OpenShift
集群的运维人员。英文版请参见
[`DEPLOY.md`](DEPLOY.md);两个文档描述的是**同一套**部署流程,
**修改任何一个时,请保持另一个同步**。

English version: [`DEPLOY.md`](DEPLOY.md). Both files describe the
same deployment procedure; please keep them in sync if you change
either.

---

## 目录

1. [部署前检查清单](#1-部署前检查清单)
2. [应用 manifest](#2-应用-manifest)
3. [SSH 密钥对的存放位置](#3-ssh-密钥对的存放位置)
4. [必须配置的 NetworkPolicy(用于拨 FRS)](#4-必须配置的-networkpolicy用于拨-frs)
5. [OpenShift SecurityContextConstraints](#5-openshift-securitycontextconstraints)
6. [部署后验证](#6-部署后验证)
7. [向导冒烟测试](#7-向导冒烟测试)
8. [清理 Failed / 残留 FRS](#8-清理-failed--残留-frs)
9. [故障排查](#9-故障排查)
10. [升级 / 修改镜像 tag](#10-升级--修改镜像-tag)
11. [自定义命名空间白名单](#11-自定义命名空间白名单)

本文档中所有步骤用以下标签区分:

| 标签 | 含义 |
| ---- | ---- |
| 🔴 **必须** | 跳过这步,**部署一定跑不起来**。一定要做。 |
| 🟡 **强烈建议** | 理论上能跑,但为了可用性和安全性,你也应该做。 |
| 🟢 **可选** | 只有非默认行为才需要。首次部署可以跳过,以后有需要再回来调。 |

---

## 1. 部署前检查清单

| # | 步骤 | 标签 |
| - | ---- | ---- |
| 1 | OCP ≥ 4.11 | 🔴 必须 |
| 2 | 已安装 Kasten K10 及 `filerecoverysessions.datamover.kio.kasten.io` CRD(用 `kubectl api-resources \| grep filerecovery` 确认) | 🔴 必须 |
| 3 | 给 helper 选一个命名空间 — 默认是 `kasten-io`(与 K10 一致)。如果用别的命名空间,**必须**同时修改 `deploy/kustomization.yaml` 的 `namespace:` 字段和 `HELPER_PRIVATE_KEY_SECRET_NAMESPACE` 环境变量。 | 🔴 必须 |
| 4 | 生成三个凭据并写入 helper 命名空间下名为 `kasten-frs-web-helper-credentials` 的 Secret 中。每个值**至少 16 字节**。 | 🔴 必须 |
| 5 | 从 <https://github.com/6547709/kasten-frs-web/pkgs/container/kasten-frs-web> 选一个 release tag(推荐最新)。首次部署时直接用 `deploy/kustomization.yaml` 里的默认值即可,以后再换。 | 🟢 可选 |
| 6 | 决定 helper 能否看到集群中所有 FRS,还是只看到指定命名空间下的 FRS(默认 = 全集群)。 | 🟢 可选 |

### 生成凭据

🔴 **必须**。运行一次,把三个值妥善保存:

```bash
PW=$(openssl rand -base64 24)   # HELPER_PASSWORD
US=$(openssl rand -base64 16)   # HELPER_USERNAME(或自己取名)
CS=$(openssl rand -base64 32)   # HELPER_COOKIE_SECRET
```

把三个值放进你的密码管理器。helper 启动时会拒绝任何长度 < 16
字节的值,以及空的 `HELPER_COOKIE_SECRET`。

### 创建凭据 Secret

🔴 **必须**。两种方式等价:

**方式 A — 直接用变量创建:**

```bash
oc -n kasten-io create secret generic kasten-frs-web-helper-credentials \
  --from-literal=username="$US" \
  --from-literal=password="$PW" \
  --from-literal=cookie-secret="$CS"
```

**方式 B — 用 YAML 文件**(用 git/SOPS/SealedSecrets 管理的
话走这种方式):

```yaml
apiVersion: v1
kind: Secret
metadata:
  name: kasten-frs-web-helper-credentials
  namespace: kasten-io
type: Opaque
stringData:
  username: "REPLACE_ME_USERNAME"
  password: "REPLACE_ME_PASSWORD"
  cookie-secret: "REPLACE_ME_COOKIE_SECRET_BASE64_32B"
```

Deployment manifest 里的 `envFrom.secretRef.name` 引用了这个
Secret 的名字;如果你改名,记得同步改 `deploy/20-deployment.yaml`。

---

## 2. 应用 manifest

🔴 **必须**。在仓库根目录执行:

```bash
oc apply -k deploy/
```

按顺序应用以下资源:

1. `05-serviceaccount.yaml` — `kasten-frs-web-helper` SA
2. `06-rbac.yaml` — Secret / ConfigMap / Pod / FRS 的 Role + RoleBinding
3. `20-deployment.yaml` — helper Deployment(1 副本)
4. `30-service.yaml` — 8080 端口的 ClusterIP Service
5. `40-route.yaml` — 对外的 edge-TLS Route
6. `50-networkpolicy.yaml` — `kasten-io` 到 API server 的 egress
7. `55-networkpolicy-helper-access-frs.yaml` — 允许 helper 拨 FRS
   2222 端口的 ingress 策略(详见 §4)

> **RBAC 注意(首次启动):** helper 在首次启动时会自动创建自己的
> SSH 密钥 Secret(详见 §3)。Kubernetes 对 `create` 动作**不
> 生效** `resourceNames`(admission 时对象名还不存在),所以
> `06-rbac.yaml` 里对 Secret 的 `create` 授权**故意**只按命名空间
> 限制、不限制名字。`get/update/patch` 仍然限制在
> `kasten-frs-helper-private-key` 这一个 Secret 上。**不要**
> 给 `create` 规则加回 `resourceNames`,否则 helper 会以
> `secrets is forbidden: cannot create` 崩溃循环。

### bundle 不会做的事

🟢 **可选**。bundle 故意不做这些事,以免和 K10 自己的 helm
管理的 `kasten-io` 命名空间打架:

- **不**创建 `kasten-io` 命名空间。K10 的 chart 拥有它。在没有
  K10 的全新集群上,先手动 `oc create namespace kasten-io`。
- **不**创建凭据 Secret。详见 §1。
- **不**给 `kasten-io` 命名空间加 privileged 标签。详见 §5。
- **不**推 SSH 密钥对 Secret。详见 §3。

---

## 3. SSH 密钥对的存放位置

🔴 **必读**(无需手动操作 — helper 自己管理)。

helper 用一对 SSH 密钥来认证 FRS 的 SFTP。公钥嵌在向导创建的
每个 FileRecoverySession 里;私钥由 helper 自己用来回拨 FRS pod。

密钥对**在 helper 首次启动时自动生成**,并以 Kubernetes Secret
形式持久化,这样 pod 重启后还是同一对。**你不需要**自己跑
`ssh-keygen`,也不需要 mount 任何密钥。

| 属性 | 值 |
| ---- | -- |
| Secret 名字 | `kasten-frs-helper-private-key` |
| Secret 命名空间 | `kasten-io`(可通过 `HELPER_PRIVATE_KEY_SECRET_NAMESPACE` 改) |
| Secret 类型 | `kubernetes.io/ssh-auth` |
| 数据字段 `ssh-privatekey` | PEM 编码的 ed25519 私钥(无口令) |
| 数据字段 `ssh-publickey` | `ssh-ed25519 AAAA…` authorized_keys 行 |

默认值定义在 `internal/config/config.go` 里:

| 环境变量 | 默认值 | 作用 |
| -------- | ------ | ---- |
| `HELPER_PRIVATE_KEY_SECRET_NAME` | `kasten-frs-helper-private-key` | 需要换名字时改这里 |
| `HELPER_PRIVATE_KEY_SECRET_NAMESPACE` | `kasten-io` | helper 不在默认命名空间时改这里 |
| `HELPER_PRIVATE_KEY_SECRET_FIELD` | `ssh-privatekey` | 改数据字段名(进阶) |
| `HELPER_USERNAME_FIELD` | `username` | 遗留字段,SFTP 登录不读它 |

### 首次启动会发生什么

1. helper 读 Secret `kasten-io/kasten-frs-helper-private-key`。
2. 没找到 → 生成一对 ed25519 密钥,两个字段都写入 Secret,
   `type: kubernetes.io/ssh-auth`,然后开始服务。
3. 找到了且两个字段都在 → 加载,开始服务。
4. 只有公钥,没有私钥 → **拒绝启动**(报错 "refusing to
   operate")。删掉 Secret 让 helper 重新生成,或者从备份恢复
   私钥。
5. 只有私钥,没有公钥 → 从私钥派生公钥并 patch Secret(幂等)。

### 怎么查公钥(在非托管 FRS 上手动注册时用)

如果你需要把 helper 的公钥手动加到一个已存在的 FRS 上(向导
会自动处理):

```bash
oc -n kasten-io get secret kasten-frs-helper-private-key \
  -o jsonpath='{.data.ssh-publickey}' | base64 -d
# ssh-ed25519 AAAA… kasten-frs-web-helper
```

### 怎么查私钥(很少用到)

正常情况下,私钥**永远不会**离开 helper pod。如果你**必须**
读它(比如灾备演练):

```bash
oc -n kasten-io get secret kasten-frs-helper-private-key \
  -o jsonpath='{.data.ssh-privatekey}' | base64 -d
```

**把这段输出当敏感信息对待。** 它就是 helper 能拨的所有 FRS
的 SSH 密钥。

### 备份 / 恢复密钥对

```bash
# 备份
oc -n kasten-io get secret kasten-frs-helper-private-key -o yaml > ssh-key.yaml

# 恢复(helper 下次启动时会用上)
oc -n kasten-io apply -f ssh-key.yaml
```

如果你删掉了 Secret,helper 下次启动会生成新密钥对。**所有正在
跑的 FRS 会失去 SFTP 访问能力** — FRS CR 还在,但 helper 没法
认证了。把残留 FRS 删掉,再用向导重建。

### 3.1 可选:手工预创建密钥对

🟢 **可选**。正常首次安装直接跳过这一节 — helper 首次启动会
自己生成密钥对。只有下面任一情况才需要看:

- helper 的 ServiceAccount **不允许 `create` Secret**(比如你
  安装后加固了 RBAC)
- **气隙 / 预烘焙**集群,helper pod 首次启动连不上 API server
  (见 §9 故障排查表里 "helper 连不上 API server" 那一行)
- 多副本 helper 想用**同一把共享密钥**(当前 Deployment 是
  1 副本,所以基本用不到 — 扩到多副本时,所有副本必须用同一
  把)
- 想把**现成的密钥**灌进去(比如从你的密钥管理平台出来的)

#### 3.1.1 生成密钥对

在任意有 `ssh-keygen` 的机器上:

```bash
ssh-keygen -t ed25519 -N '' -f kasten-frs-helper -C kasten-frs-web-helper
# kasten-frs-helper       <- 私钥
# kasten-frs-helper.pub   <- 公钥
```

helper 接受任何 ed25519 / RSA 密钥 — 推荐 ed25519,更小。**不要
设口令**(`-N ''`):helper 没法在无人值守时用有口令的密钥。

#### 3.1.2 创建 Secret

把两段都 base64 包一下,在 helper 命名空间里建一个
`kubernetes.io/ssh-auth` Secret:

```bash
NS=kasten-io                                # helper 命名空间
NAME=kasten-frs-helper-private-key          # Secret 名(默认)
PRIV=$(base64 -w0 kasten-frs-helper)        # 私钥的 base64
PUB=$(base64 -w0  kasten-frs-helper.pub)    # 公钥的 base64

oc -n $NS create secret generic $NAME \
  --type=kubernetes.io/ssh-auth \
  --from-file=ssh-privatekey=kasten-frs-helper \
  --from-file=ssh-publickey=kasten-frs-helper.pub
```

> `--type=kubernetes.io/ssh-auth` 对我们 helper 来说只是标注
> 意图(我们直接读 data 字段),但留着能跟 helper 自己建的类型
> 保持一致。

验证两个字段都进去了:

```bash
oc -n $NS get secret $NAME -o jsonpath='{.data.ssh-privatekey}' | base64 -d
# -----BEGIN OPENSSH PRIVATE KEY-----
# ...
oc -n $NS get secret $NAME -o jsonpath='{.data.ssh-publickey}'  | base64 -d
# ssh-ed25519 AAAA… kasten-frs-web-helper
```

#### 3.1.3 预创建之后行为的变化

helper 首次启动的 `LoadOrGenerate` 路径会变成:

1. `secrets.Get` → 成功(Secret 已经在了)。
2. `ssh-privatekey` 和 `ssh-publickey` 都在 → 走 `parseInto`,
   **不会**再尝试 `create` Secret。
3. helper 用你给的密钥启动。

也就是说 helper 不再需要 Secret 的 `create` 权限。如果安全团队
希望收紧到只 `get/update/patch`,**可以**把 `06-rbac.yaml` 里
的 `create` 规则删掉,helper 照样能起来。

#### 3.1.4 重要提醒

- **Secret 里必须两段都在。** 只有公钥的话,helper **拒绝启动**
  (报 "refusing to operate"),要么补回私钥,要么删掉 Secret
  让 helper 自己重新生成。
- **所有 helper 副本必须指向同一个 Secret。** `replicas: 1` 时
  天然就这样。如果将来扩到多副本,千万不要让每个副本各自生成
  自己的密钥 — 那会把正在跑的 FRS 全搞挂。
- **预创建之后,密钥备份是你自己的责任。** 自动生成的路径也
  是把密钥放在 Secret 里,但自动生成天然给了一次性引导。预创建
  进来的密钥跟别的 Secret 一样需要备份 — 见上面的"备份 / 恢复
  密钥对"。

---

## 4. 必须配置的 NetworkPolicy(用于拨 FRS)

🔴 **必须**。已经在 bundle 的
`55-networkpolicy-helper-access-frs.yaml` 里;本节解释为什么
需要它。

K10 datamover controller 会为每个 FRS 创建一个
`NetworkPolicy`,ingress 源限制在 app 所在的命名空间(比如
`default`)。因为 helper pod 跑在 `kasten-io`,这些策略会挡住
对 FRS 2222 端口的 SFTP 拨测,导致创建 FRS 后浏览器一直卡在
`i/o timeout`。

bundle 自带一个策略,把每个 K10 gen-1 FRS pod 的 ingress 放行到
helper pod。`oc apply -k deploy/` 会应用它。如果不用 kustomize
bundle,直接复制下面的 YAML 应用:

```yaml
apiVersion: networking.k8s.io/v1
kind: NetworkPolicy
metadata:
  name: kasten-frs-web-helper-allow-all-frs
  namespace: kasten-io
spec:
  podSelector:
    matchLabels:
      k10.kasten.io/frs-generation: "1"
  ingress:
  - from:
    - podSelector:
        matchLabels:
          app: kasten-frs-web-helper
    ports:
    - port: 2222
      protocol: TCP
  policyTypes:
  - Ingress
```

**验证:** 向导创建 FRS 之后,点进目录树。如果拨测卡在
`i/o timeout`,就是这条策略没生效 — 应用它再重试。

---

## 5. OpenShift SecurityContextConstraints

🟡 **强烈建议**。

涉及两个不同的 pod;只有 K10 那一个需要 privileged SCC。

### 5.1 helper pod(不需要 SCC)

`kasten-frs-web-helper` Deployment 自带标准的 restricted-v2
兼容的 securityContext:

- `runAsNonRoot: true`
- `allowPrivilegeEscalation: false`
- `capabilities.drop: [ALL]`
- `readOnlyRootFilesystem: true`
- 没有 hostPath、没有 privileged 容器、没有特殊卷

OCP 默认的 `restricted-v2` SCC 原样接受这套配置。helper 的 SA
`kasten-frs-web-helper` 在 `kasten-io` 命名空间通过
`system:authenticated` 组自动继承这个 SCC,所以 **helper 不需要
额外的 SCC 授权**。

### 5.2 FRS mounter pod(K10 datamover — 需要 privileged)

K10 datamover 用一个 privileged 容器挂载 FRS 数据
(`securityContext.privileged=true` + hostPath +
`capabilities.add: SYS_ADMIN`)。OCP 的 `restricted-v2` SCC 默认
拒绝这种配置。helper bundle 自己没法授权 SCC,但你需要确保 K10
controller 能跑起它的 FRS mounter pod。

两种方式:

**方式 A — 把 `kasten-io` 命名空间标为 privileged**
(最简单,符合 K10 上游文档的推荐):

```bash
oc label namespace kasten-io \
    pod-security.kubernetes.io/enforce=privileged \
    pod-security.kubernetes.io/audit=privileged \
    pod-security.kubernetes.io/warn=privileged \
    --overwrite
```

不标的话,每个向导 / k10tools 创建的 FRS 都会以
`state=Failed` 收尾,带 `CreatedPod: violates PodSecurity
"restricted:latest"`,SFTP 拨测也连不上。

**方式 B — 给 K10 datamover 的 SA 授 privileged SCC**
(在创建 FRS 的命名空间里,通常是 `default`):

```bash
oc adm policy add-scc-to-user privileged -n default -z default
```

除非安全团队硬性要求更细的粒度,否则**优先选方式 A**。

---

## 6. 部署后验证

🔴 **必须**,投产前必做。

```bash
HELPER_POD=$(oc get pod -n kasten-io -l app=kasten-frs-web-helper -o jsonpath='{.items[0].metadata.name}')
oc wait --for=condition=Ready pod/$HELPER_POD -n kasten-io --timeout=60s

# 1. API 可达
oc exec -n kasten-io $HELPER_POD -- nslookup kubernetes.default
oc exec -n kasten-io $HELPER_POD -- curl -sk https://kubernetes.default.svc/api

# 2. FRS 拨测(把 frs-xxx 换成实际 FRS service 名)
oc exec -n kasten-io $HELPER_POD -- bash -c "timeout 3 bash -c '</dev/tcp/frs-xxx.kasten-io.svc.cluster.local/2222'"

# 3. 或者用脚本
./bin/check-netpol.sh frs-xxx kasten-io
```

`/healthz` 和 `/readyz` 可以直接打:

```bash
ROUTE=$(oc get route -n kasten-io -l app=kasten-frs-web-helper -o jsonpath='{.items[0].spec.host}')
curl -sk https://$ROUTE/healthz   # 200
curl -sk https://$ROUTE/readyz    # 200
curl -sk https://$ROUTE/login     # 200,返回 HTML
```

登录页 footer 应该显示 `Kasten FRS Web Helper · vX.Y.Z`。如果
显示 `v0.0.0` 或 `dev`,说明构建时 `VERSION` build-arg 没传进去
(详见 §10)。

---

## 7. 向导冒烟测试

🔴 **必须**,投产前必做。

helper pod Ready 之后,从 Route 登录,进入 `/wizard`。应该至少
看到一个 VM(假设 K10 里有 `virtualMachine` 标签的 RestorePoint)。
依次选 VM、选 Bound RP、选 volume,点 **Create FRS**(按钮现在靠
右,跟从左到右的向导流程一致)。应该在 120 秒内(默认
`HELPER_FRS_WAIT_TIMEOUT`)被重定向到 `/browse`,看到 FRS 的
目录树。

如果 prepare 页面在 120s 后显示 `Failed` 或 `Timeout`,看 §9。

---

## 8. 清理 Failed / 残留 FRS

🟡 **强烈建议**,作为日常维护的一部分。

Sessions 页面会列出集群里**所有** FRS(受可选的
`HELPER_FRS_NAMESPACES` 白名单影响),包括终态的。FRS 超时后会
变成 `Failed`,K10 会把它的 `frs-xxx` pod 拆掉,但 FRS 自定义
资源还在。这些行显示一个禁用的 **Unavailable** 按钮和一个可用
的 **Delete** 按钮 — 点 Delete 就能清掉残留的 CR。ClusterRole
已经授了 `filerecoverysessions` 的 `delete` 权限,不需要额外
RBAC。确认用的是应用内对话框(不再用浏览器的 `window.confirm`
弹窗)。

---

## 9. 故障排查

| 症状 | 可能原因 | 修法 |
| ---- | -------- | ---- |
| helper pod `CrashLoopBackOff` 报 `secrets is forbidden: cannot create` | 给 Secret `create` 授权又加回了 `resourceNames` | 看 §2 的 RBAC 注意。 |
| helper pod `CrashLoopBackOff` 报 `secrets "kasten-frs-helper-private-key" has public key but no private key` | 之前的运维只把公钥放进了 Secret | `oc -n kasten-io delete secret kasten-frs-helper-private-key`,让 helper 重新生成。 |
| **helper pod `CrashLoopBackOff` 报 `fatal: load/generate SSH key: get secret ...: dial tcp 172.30.0.1:443: i/o timeout`** | **pod 连不上 Kubernetes API server。** 这是 **egress** 问题(NetworkPolicy 拦了 helper 命名空间的出向、命名空间在不可路由的子网、或者集群的 private control plane 那个子网路由不到)。`LoadOrGenerate` 的第一步 `secrets.Get` 就超时了,根本走不到创建那一步。 | **先修 egress 路径** — 放行 helper 命名空间到 API server 的出向(OCP 默认就放开了,除非你额外加了 NetworkPolicy 拦掉)。如果环境上确实修不了 egress,看 §3.1 "可选:手工预创建密钥对" — 但**注意**,预创建 Secret 解决不了这个具体的报错,因为 helper 启动时第一次动作就是同一个 `secrets.Get`,照样会超时。**真正的修法是 egress**,其它都只是表面文章。 |
| 点 Create FRS 之后浏览器卡在 `i/o timeout` | §4 NetworkPolicy 没生效 | 重新 `oc apply -k deploy/` 或直接应用 §4 的 YAML。 |
| 每个向导 FRS 一上来就是 `Failed` | §5.2 SCC 没授 | 应用 §5.2 的方式 A 或 B。 |
| 找不到 `Wait longer` 按钮 | v0.3.37 已经把这个按钮删了 — 详见 v0.3.37 changelog。用"返回会话列表"或者"取消并删除 FRS"代替。 | n/a |
| 登录页 footer 显示 `dev` | `VERSION` build-arg 没传进去 | 重新构建镜像,加 `--build-arg VERSION=vX.Y.Z`;详见 §10。 |
| helper 第一次用得好好的,突然新建的 FRS 鉴权失败 | 有老 FRS 还引用着**旧**的 SSH 密钥(运维轮换了密钥对,或者有人删了 Secret 让 helper 自动生成了新密钥) | 要么把旧密钥对写回 Secret(记得先备份 — 见 §3),要么把老 FRS 全删了再用向导重建。 |

其他问题看设计 spec(`docs/superpowers/specs/`)和实施计划
(`docs/implementation_plan.md`)。

---

## 10. 升级 / 修改镜像 tag

🟢 **可选**(只在升级时用)。

镜像 tag 在 `deploy/kustomization.yaml` 的 `images:` 里集中管理。
要发布特定 release:

1. 编辑 `deploy/kustomization.yaml`:

   ```yaml
   images:
   - name: ghcr.io/6547709/kasten-frs-web
     newTag: v0.3.29        # <-- 改成新版本
     # newName: registry.internal/mirror/kasten-frs-web   # 可选镜像
   ```

2. (可选)把 `deploy/20-deployment.yaml` 里 fallback 的 `image:`
   也对齐。这条是**不用** kustomize 时(`oc apply -f
   deploy/20-deployment.yaml`)用的值;走 kustomize 的话,
   `kustomization.yaml` 里的永远赢。

3. 应用: `oc apply -k deploy/`。Deployment controller 滚动升级,
   全程 helper 都在线。

`20-deployment.yaml` 里的 `image:` 行**只是**不用 kustomize 时的
fallback。**始终用 `oc apply -k deploy/`。**

### 自行构建镜像(进阶)

🟢 **可选**。

```bash
docker build --build-arg VERSION=v0.3.29 -t registry.internal/kasten-frs-web:v0.3.29 .
docker push registry.internal/kasten-frs-web:v0.3.29
```

然后二选一:

- 镜像到 `ghcr.io` 并改 tag,或
- 在 `deploy/kustomization.yaml` 设 `newName:` 指向你的私有
  registry。

---

## 11. 自定义命名空间白名单

🟢 **可选**。

默认情况下,helper 列出**全集群**的 FileRecoverySession(不包括
`kube-system` 这类)。如果要限定到指定命名空间,设
`HELPER_FRS_NAMESPACES`:

```bash
oc -n kasten-io set env deploy/kasten-frs-web-helper \
  HELPER_FRS_NAMESPACES=default,kasten-io,vm-apps
```

Sessions 页面就只显示这些命名空间下的 FRS。列表用英文逗号分隔,
不要有空格。自动重启(env 变更会触发滚动更新)。

---

*最后审核: 2026-06-17。修改"必须"步骤时,请同步更新
`DEPLOY.md`(英文版)。*
