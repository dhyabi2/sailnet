> 本文是 [README.md](README.md) 的简体中文翻译；如有出入，以英文版 README.md 为准。

# Sailnet

**网站与下载：[www.sailnet.space](https://www.sailnet.space)**

一个以 XNO 支付报酬的隐私网络。relay 通过看起来普通的 HTTPS 承载 onion 路由的
线路（circuit），按每兆字节获得报酬，直接在 Nano 账本上结算。没有代币，没有账户，
中间没有公司。

- **运行 relay**：在任何 VPS 或家用电脑上运行，把 XNO 收入打到你自己掌控的钱包。
- **使用网络**：通过 Android 应用、桌面应用（为任意浏览器提供 SOCKS5 代理
  和 DNS），或 Chrome 与 Firefox 扩展。
- **抗审查**：bridge、WebSocket 形态的隧道、经过实测的
  流量整形。

下载：[Releases](../../releases) 页面提供适用于 Linux（x86-64 和 arm64）的
`sailnode` 和 `sail`、macOS 与 Windows 桌面应用、Android
APK 以及 Chrome 和 Firefox 扩展，每个文件旁都附有一个 `.sha256`。
Docker 镜像位于 GitHub Container Registry，名为 `ghcr.io/dhyabi2/sailnet`。

## 一条命令运行 relay

> ### 先设置 `--payout`，在做任何其他事情之前
>
> `--payout nano_…` 是你的收入将被发送到的地址，也是你应当在启动 relay 之前
> 而不是之后做出的那个决定。
>
> **不设置它，relay 照样赚钱——但钱进入的是一个只存在于那台服务器上的
> 钱包。** 只要机器还在，就什么都不会丢失，但种子（seed）存放在
> `SAIL_HOME/wallet.json` 中，别无他处：没有副本，没有账户，没有客服
> 地址。销毁 VPS、丢失磁盘，或在没有导出该种子的情况下重建机器，
> 钱就没了，永久消失，谁也无法把它还给你。这与让网络无法被冻结的
> 是同一个特性。
>
> 设置了 `--payout` 后，超出一小笔运营备用金的所有金额，每 15 分钟就会被转出
> 到一个你已掌控的钱包，因此服务器上持有的从不超过几分钟的收入，
> 随时都可以丢弃。
>
> 使用任何你拥有的 Nano 地址——Natrium、Nault、交易所充值地址都可以。
> 设置它不花任何费用，也不存在设置得太早的问题。
>
> **已经在没有它的情况下运行了？** 目前还没有丢失任何东西。备份钱包，或者
> 把收入转出并加上这个参数。注意 `SAIL_WALLET=` 前缀：在
> relay 上运行时，`sailnode wallet` 会找到*已安装服务*所用的钱包，
> 而单独的 `sail` 会去找 `~/.sail`，从错误的（很可能是
> 空的）钱包发送。
>
> ```
> sailnode wallet export                     # 保存你收入的种子和地址——把它抄下来
> W=$(sailnode wallet where 2>/dev/null)     # 服务实际使用的路径
> SAIL_WALLET=$W sail wallet show            # 已累积的金额
> SAIL_WALLET=$W sail send nano_your_wallet 0.5
> # 然后把 --payout nano_your_wallet 加到 unit 中，并执行：systemctl restart sailnode
> ```

Docker：

```
docker run -d --name sailnet --restart unless-stopped -p 443:443 -v sailnet:/data \
  ghcr.io/dhyabi2/sailnet relay --register --payout nano_your_wallet_address_here
```

二进制文件（Linux，x86-64 或 arm64，以 root 身份，端口 443；会校验 checksum）：

```
arch=$(uname -m | sed 's/x86_64/amd64/; s/aarch64/arm64/')
curl -fsSL -o /usr/local/bin/sailnode https://github.com/dhyabi2/sailnet/releases/latest/download/sailnode-linux-$arch \
  && curl -fsSL https://github.com/dhyabi2/sailnet/releases/latest/download/sailnode-linux-$arch.sha256 | sed 's#  .*#  /usr/local/bin/sailnode#' | sha256sum -c \
  && chmod +x /usr/local/bin/sailnode
sailnode relay --register --payout nano_your_wallet_address_here
```

命令是 `sailnode`——Sailnet 是网络的名称，不是一个二进制文件。发布版附带的
另一个命令是 `sail`，即钱包命令行工具。

要让它在重启后继续运行：

```
cat > /etc/systemd/system/sailnode.service <<'EOF'
[Unit]
Description=Sailnet relay
After=network-online.target
[Service]
Environment=SAIL_HOME=/var/lib/sailnode
ExecStart=/usr/local/bin/sailnode relay --register --payout nano_your_wallet_address_here
Restart=always
RestartSec=5
KillSignal=SIGTERM
TimeoutStopSec=30
AmbientCapabilities=CAP_NET_BIND_SERVICE
[Install]
WantedBy=multi-user.target
EOF
systemctl enable --now sailnode
```

`SAIL_HOME` 是节点存放其钱包和状态的位置。按上面的方式在
unit 中设置它：systemd 启动服务时没有 `HOME`，而一个只能靠猜的节点
会选一个你想不到去找的目录。Docker 已经把它设为
`/data`。

节点首次启动时会在那里创建自己的钱包，此后一直复用它
——重新安装、升级或重启都不会在已有种子之上再生成第二个种子。
**备份它**（`sailnode wallet export`）：
这些收入没有其他副本，这正是上文 `--payout` 的意义所在。
空钱包会自行向 Sailnet faucet 申请注册所需的金额，
因此无需手动发送任何东西；如果 faucet 不可用，
日志会写明金额（0.0005 XNO）和地址。向它所转发到的 relay 的预付款来自它自己的
收入。`sailnode relay -h` 列出所有参数；常用的有：

| 参数 | 作用 |
|---|---|
| `--payout nano_…` | **先设置这个。** 把收入转到你掌控的钱包，每 15 分钟检查一次。不设置时，收入累积在服务器上的 `SAIL_HOME/wallet.json` 中，而那个种子是世上唯一的副本 |
| `--ip 203.0.113.7` | 发布在账本上的公网 IPv4；省略时自动检测 |
| `--cc DE` | 发布在账本上的 ISO 国家代码，使客户端可以选择跨国家的路径并按国家选择出口；可选（`XX`） |
| `--owner nano_…` | 一个可以免费使用此 relay 的钱包，手动设置。默认：`--payout`。从应用配对（`sailnode pair`）可以添加更多。参见 *运行 relay，免费使用它* |
| `--spot-discount 50` | 以更低价格出售闲置容量：折扣百分比，在负载低于 `--spot-below`（默认为 `--capacity-mbps` 的 20%）时，以 30 分钟为窗口签名写入 gossip。默认关闭。参见 *清闲时更便宜* |
| `--rep-friends nano_…,nano_…` `--rep-bonus 20` | 对其账户投票给你所认可的代表（representative）的付款方，每 XNO 提供更多字节。你的看法，以价格的形式表达。默认关闭 |
| `--payout-keep 0.5` | 留在节点上、用于预付下一跳的 XNO 备用金；超出部分全部转出。不设置时它会自行确定大小：按对等节点公布的价格，八次资金池充值的费用。它跟随预付的实际成本，提高你自己的 `--rate` 不会改变它——你收多少钱不应决定你何时拿到钱 |
| `--rate 0.0005` | 起始价格，单位为每 MiB 的 XNO（约每 GB 0.20 美元）；`--reprice` 每 10 天根据需求调整它：使用量下降时降 10%，增长时升 3%，绝不超过起始价的四倍。更改此参数会覆盖需求此前对价格所做的一切调整 |
| `--min-rate 0.0002` | `--reprice` 永远不得低于的价格下限（默认：`--rate` 的四分之一）。把它设为你提供每 MiB 服务的实际成本，从那以后价格就会自行调整 |
| `--pool-mib 32` | 每次向下一跳预付所购买的服务量（MiB），按该 relay 自己公布的价格计算。以服务量而非 XNO 计量，因此收费更高的 relay 依然会被预付，而不是被悄悄跳过 |
| `--min-credit-mib 10` | 一次付款可以开通的最小配额，每个付款钱包每天授予一次。它让基于较旧、较便宜价格构建的应用仍可在你的 relay 上使用，而不是让它陷入反复重新锚定（re-anchor）的循环；`0` 表示关闭 |
| `--host relay.example.org --acme` | 一个真实域名和自动获取的 Let's Encrypt 证书 |
| `--listen :443,:8443` | 额外端口，以 bridge line 的形式打印出来 |
| `--unlisted` | 作为 bridge 运行：从不出现在账本上，通过邀请分发 |
| `--exit=false` | 仅作为中间 relay |
| `--rpc http://127.0.0.1:7076` | Nano RPC 端点，用逗号分隔，按顺序尝试；默认是 Sailnet 的端点，然后是公共节点 |
| `--rpc-key …` | API 密钥，仅当 `--rpc` 为 rpc.nano.to 时需要 |

## 运行 relay，免费使用它

如果你运行一个 relay，你自己的应用使用它时无需付费。relay 会指定一个
所有者（owner）——即其收入流向的钱包（`--payout`），或你用
`--owner nano_…` 指定的钱包——该钱包在这个 relay 上开启的线路
仅凭签名即被接纳：无需付款，无需查询账本。它之后的各跳
像所有线路一样，由该 relay 的备用金预付，因此你的流量是
用你的 relay 赚到的钱支付的，而那本来就是你的钱。没有任何
别人的 relay 会承载未付费的内容；其他每个运营者赚到的都恰好是
他们本应赚到的。

**配对——六位数字，无需复制其他任何东西。** 在 relay 上：

```
sailnode pair
Pairing code:  483 920      (valid five minutes, one use)
```

在应用中：设置 → *Run a relay, ride it free* → **Add my relay** → 选择
该 relay，输入代码。*Paired relays* 列出这部手机已配对的 relay，
并且可以移除它们；无需手动输入任何内容。应用向该 relay 支付一个普通的锚定（anchor）款项，开启
一条到它的线路，并在线路内发送代码；relay 把付款的
钱包记录为所有者。尚无所有者的 relay 在启动时会自行打印一个代码
（`docker logs sailnet` 可以看到）。在命令行中，同样的操作是
`sailnode pair-relay nano_<relay> 483920`，然后 `--mine nano_<relay>`。

然后选择如何使用你的 relay——设置 → **Network**：

| 模式 | 路径 | 你支付 | 谁能看到你的地址 |
|---|---|---|---|
| **Direct** | 仅你的 relay，1 跳 | 不付费 | 你的 relay（及其主机）；网站看到的是该 relay。相当于通过你自己拥有的机器使用的 VPN |
| **My relays** | 陌生人 → 陌生人 → 你的 relay | 一笔入口锚定款 | 仅入口；监视你的 relay 的人看不到你到达 |
| **Open network** | 三个陌生人 | 全价 | 仅入口；不使用你的 relay |

Direct 适用于拥有一个 relay、不想付钱给两个陌生人的所有者；
My relays 适用于你不愿被人看到自己到达自己的 relay 的情况。三种模式
使用相同的链路整形：在实际运行的 relay 上测得，隐藏链路节奏的掩护节拍
同时也是更快的链路（没有它的配置传输 20 MB 时速度只有三分之一），
因此没有什么需要关闭的。
在已连接状态下更改 *Network* 或配对 relay 时，隧道会自行重新连接，
因此设置会立即生效。如果
在 Direct 模式下你的 relay 宕机，应用会如实告知，并提供另外两种模式，而不是
悄悄付费。`--owner nano_…`（默认 `--payout`）对于你在 relay 上自行设置的钱包
仍然和以前一样有效。所有者标签绑定于其所针对的
relay，并用你的密钥签名，因此在其他任何地方都没有意义，
而且除你的 relay 之外，在所有人看来这条线路与其他线路并无二致。

Nano 爱好者多年来一直无偿运行代表节点。这是同样的
安排，只是其中有你的一份好处：你帮助承载的网络，正是
你浏览时所经过的网络。

## 查看你的 relay

节点会在你启动它的终端中持续运行，并且不读取
你在那里输入的内容。打开**第二个终端**并运行：

```
sailnode stats             # 此 relay 的当前状态：已中继的 MiB、开启的线路、运行时间，以及它所看到的网络
sailnode stats --watch 2   # 同上，每两秒刷新一次，并显示速率
```

如果你直接运行下载的文件，而不是将其安装为
`sailnode`，命令就是 `./sailnode-linux-amd64 stats`（或该文件的
实际名称）。

它查询 relay 自身在回环地址上的 `/stats`——一个 HTTPS 请求，从
内存中读取。不涉及账本，不涉及 RPC，不在网络上产生任何流量。如果 relay
不监听 `127.0.0.1:443`，请使用 `--addr`。暂不支持 `earn --home` 节点：家用节点
没有本地端口，因此 `stats` 无处可查；但它照样在
赚钱。

## 清闲时更便宜，并优待良好的 Nano 公民

relay 可以单方面开启的两项功能。两者都不需要其他任何人同意，
也都不会让任何 relay 以低于其要价的价格承载哪怕一个字节。

**现货价格（Spot price）。** `--spot-discount 50` 让空闲的 relay 在接下来的三十分钟内，
把一个更低的价格签名写入它自己的 gossip 记录；看到该
报价的客户端按这个价格付款，relay 在整个窗口期内都按此价格计入额度，无论忙
与不忙。这是 relay 本来每十天就会进行的重新定价的快速版本，
而且这是 relay 对自己价格的签名，因此客户端可以
要求它兑现。`sailnode relays` 显示当前有效的报价。

**与代表一致的价格（Representative-aligned price）。** `--rep-friends nano_…,nano_… --rep-bonus 20`
为其账户投票给你所列代表之一的付款方，每 XNO 多计入五分之一的字节。
投票信息就在 relay 本来就会读取的付款区块中，因此检查它不花任何成本，
也不会泄露付款本身未泄露的任何信息。它只改变每 XNO 的字节数——绝不改变谁会被选中。

**计量表（The meter）。** `sailnode costs`（以及应用的主屏幕）显示这个
钱包付了多少钱、付给了哪个 relay、按什么价格，以及其中用掉了多少——数据来自
一个仅存在于设备上、别无他处的账本。

## 升级

```
sailnode upgrade            # 获取最新发布的构建，校验，安装，重启
sailnode upgrade -check     # 告知你是否已是最新版本，不做任何更改
sailnode upgrade -restart=false   # 立即安装，在你选择的时候重启
```

下载内容会与其旁边发布的 SHA-256 进行比对，在匹配之前不会替换任何东西，
因此下载失败或被篡改时，正在运行的节点不受影响。之前的二进制文件
会以 `sailnode.previous` 的名称保留在新文件旁边。你的钱包、你的配额日志以及
`SAIL_HOME` 中的其他所有内容，升级时都绝不会被读取或写入。

然后，在重启服务之前，新构建会先运行一次。一个
无法执行的二进制文件——架构错误、连同其 checksum 文件一起被截断的下载、
缺少某个库——否则就只能由一个不断重启的服务来发现，
而这正是运营者不会盯着看它发生的那种故障。如果它无法运行，
就恢复之前的构建，不重启任何东西。`sail`（钱包命令行工具）如果
已经安装在 `sailnode` 旁边，会同时升级；升级绝不会添加你
未曾选择拥有的命令。

设置 `SAIL_RELEASE_API` 可以从别处升级——分叉、镜像、
物理隔离（air-gapped）的副本。下载内容仍会与其旁边发布的 `.sha256` 进行校验，
因此把它指向别处只是换了一个来源，而不是一个更弱的来源。

升级是可选的。一个从不升级的 relay 会继续工作，继续被
选入线路，继续赚钱：网络的设计使得任何
发布版本都不能要求运营者采取行动。

**Nano RPC。** 客户端和 relay 默认通过
`https://www.sailnet.space/node/api` 读取账本，这是 Sailnet 自己的端点，
它使用 Sailnet 的密钥转发到 rpc.nano.to，并在故障时切换到公共
节点。应用的设置允许你用自己的密钥直接使用 rpc.nano.to，
或使用你运行的任何节点；命令行工具接受 `--rpc` 和 `--rpc-key`（或 `NANO_RPC_URLS` 和
`NANO_RPC_KEY`）。

relay 开箱即可通过 Sailnet 的端点工作；无需安装 Nano 节点。
如果你仍然运行了一个，请传入 `--rpc http://127.0.0.1:7076`。

位于 NAT 之后、没有端口转发的家用电脑：`sailnode earn --home --payout nano_…`。

## 升级应用

**Relay（Linux）。** `sailnode upgrade` 就是全部步骤。它在替换任何东西之前
校验发布的 SHA-256，把之前的二进制文件保留在新文件旁边，
并且绝不触碰你的钱包或 `SAIL_HOME` 中的任何其他文件。
升级是可选的：一个从不升级的 relay 会继续工作，继续被
选中，继续赚钱。

**桌面（Windows、macOS）。** 下载并覆盖安装。钱包
会保留下来，但它不在 `~/.sail` 中：应用把它保存在操作
系统的配置目录中——macOS 上为 `~/Library/Application Support/Sailnet`，
Windows 上为 `%AppData%\Sailnet`，Linux 上为 `~/.config/Sailnet`——
与其设置放在一起，重新安装触及不到那里。`~/.sail` 属于
命令行（`sailnode`、`sail`）：在那里充值的钱包并不是
应用打开时使用的那个，`sail wallet export` 也看不到应用的
钱包。请通过 设置 → Wallet → Back up wallet 备份那个钱包。

**Android——一次性重新安装。** v0.2.53 之前的每个版本都使用
每次构建都会变化的一次性密钥签名，因此 Android 拒绝在旧版本之上安装
新版本。升级到 v0.2.53 需要卸载后
重新安装，仅此一次。如果钱包中有任何你想保留的东西，**请先备份你的种子**（设置 → Back up wallet）
——卸载会删除它。从
v0.2.53 开始，每个版本都使用同一个密钥签名，可以正常覆盖安装
上一个版本。

## 你的钱包及其备份

relay 的收入和客户端的余额都存在于同一个秘密中：一个 32 字节的种子，
位于 `wallet.json` 中。没有其他人拥有副本。没有可以找回的账户，
也没有能把它还给你的客服地址，因为一个有人能够
把你的钱还给你的网络，也是一个有人能够把它拿走的网络。

所以，把它抄下来。

```
sailnode wallet export      # 显示要保存的地址和种子
sailnode wallet import SEED # 恢复已保存的钱包（也接受文件，或 -）
sailnode wallet where       # 它的存储路径
```

在 relay 的 shell 中运行时，这些命令会找到已安装服务
实际使用的钱包，而不是你的登录账户会用的那个：会读取 unit
以获得其数据目录。不做任何猜测，显式指定的 `SAIL_HOME` 或
`SAIL_WALLET` 始终优先。

应用中也有同样的两个操作。在 Windows、macOS 和 Linux 上，它们位于
设置中的 **Wallet** 下：*Back up wallet* 显示种子，并可将其保存到
文件，*Restore wallet* 则将其恢复。在 Android 上它们同样位于设置中的
**Wallet** 下，而且在那里最为重要：卸载 Android 应用
会删除它存储的一切，包括这个钱包。

以下两项保证在任何地方都成立：

- **已有的钱包总是会被复用。** 再次安装、升级，或
  第十次启动 relay，都绝不会在旧种子之上生成新种子。
  存在但无法读取的钱包文件会原样保留并
  报告，绝不会被替换。
- **恢复操作会保留被替换的内容。** 原有的钱包会被重命名为
  `wallet.json.replaced-<time>`，放在新钱包旁边，因此从
  错误的纸条上输入的种子并不意味着一切就此结束。

请在隧道断开时进行恢复。已经开启的线路是由
你正在替换的那个钱包预付的。

## 使用网络

```
sailnode client --socks 127.0.0.1:1080 --hops 3
curl --socks5-hostname 127.0.0.1:1080 https://api.ipify.org
```

客户端在首次运行时会创建一个钱包，并为其申领一笔免费试用额度，
因此第一条线路不花任何钱，也没有任何一步要求你在看到它是否可用之前
先购买 XNO。此后它自己承担费用：它按每个入口 relay
公布的价格为其预付约 10 MiB，并根据线路的实测速率
续费。bridge 和抗审查防御
始终开启，没有开关；来自运行 bridge 的人的 bridge line
是唯一值得手动添加的东西。`sailnode client -h` 列出整机
流量捕获、经由线路的 DNS，以及供浏览器
扩展使用的状态端点。Android：从 Releases 安装 APK；应用会以同样的方式
为自己充值，并显示在哪里获取 XNO。

在 Android 上，有两项设置可以节省 XNO 和电量，或分享连接：

- **Pause when screen off**（默认开启）：屏幕关闭时，
  线路关闭，不进行任何 ping，也不为任何东西付费；隧道保持开启并
  丢弃流量，因此不会泄露任何内容。位于 Sailnet 前台的其他应用会继续使用
  隧道。解锁后通过同一个入口恢复，使用
  已预付的锚定款。
- **Share connection**：其他设备通过热点使用你的线路。
  Android 的热点从不经过 VPN 应用，因此访客需要在其 Wi-Fi 设置中
  设置代理（`<hotspot address>:8080`，HTTP 或 SOCKS5）。
  *Sailnet hotspot* 会开启它自己的网络并显示其二维码；它本身没有
  互联网连接，因此未设置代理的访客什么也访问不到。
  *My hotspot* 使用 Android 的热点，其名称和密码保持不变，
  但未设置代理的访客会在无保护的情况下直接出网。访客只能访问公共
  HTTPS，绝不能访问这部手机或其局域网，并由你的
  钱包付费。分享会在屏幕关闭时保持隧道开启。

## 为难以审查而构建

以下每一项如今都已在代码中实现，而不是计划。

- **始终只有一个连接。** 客户端只与其入口 relay 通信。
  新钱包的第一次账本读取，以及对其第一笔
  付款的监视，都经由入口、通过隧道的控制通道进行：入口
  转发一小组限速的账本请求，并在确认到达的那一刻
  推送给客户端。客户端从不联系任何 Nano 节点、任何网站、任何 WebSocket，
  并且每个区块都在客户端上签名。
- **入口链路上的节拍。** 客户端到入口连接的两端
  每 25 ms 至少发送一个 cell，空闲时用填充数据，因此链路的节奏
  不再跟随用户的操作；relay 以随机化的时序进行批处理，
  使单个流在汇总中失去其形态。
- **拒绝明文 HTTP。** 除非运营者明确允许，端口 80 的流量
  从不会离开出口；只有加密的目的地可以。
- **看起来像一个网站。** relay 是一个带有真实诱饵站点的 HTTPS 服务器。
  客户端发送当前 Chrome 的 ClientHello（uTLS），以 relay 的
  主机名作为 SNI，然后进行标准的 WebSocket 升级；只有携带
  由 relay 密钥派生的每日令牌的路径才会开启隧道。任何其他请求，
  包括重放或猜测路径的主动探测，都会得到诱饵页面和
  一个完全相同的 404。
- **真实证书。** `--acme` 为真实域名获取 Let's Encrypt 证书，
  因此没有任何自签名证书；线路握手会用 relay 的账本密钥对
  当前叶证书的 SHA-256 签名，因此伪造证书的
  中间设备会在隧道内被发现，而不会被信任。
- **真实的 WebSocket 分帧。** cell 在 RFC 6455 帧中传输，因此跟踪
  握手的检查者看到的是格式正确的 WebSocket，并且 bridge 可以置于
  支持 WebSocket 的 CDN 或反向代理之后。
- **经过实测而非假设的流量整形。** 隧道写入器
  以静默间隙合并的方式批处理 cell，写入满尺寸的 TLS 记录，暂缓
  不足一个记录的剩余部分，并依据真实 HTTPS 的配置文件重放连接的
  最初几个记录。`sailtrace` 按 DPI 设备所见的方式，捕获 TLS 之下的
  记录级轨迹，在已公开的
  TLS-in-TLS 特征集加上封装握手规则上训练随机森林，而发布的
  参数正是在实际网络上针对它调优得到的。
- **没有固定节奏。** keepalive 带有抖动；不存在可供识别的
  周期性模式。
- **带有密钥的 bridge。** `relay --unlisted` 从不接触账本，
  并分发一个带有 16 字节秘密（secret）的 bridge line；隧道令牌由
  密钥和该秘密共同派生，因此读过账本并拥有
  地址的人仍然只会得到诱饵。relay 可以监听多个端口；额外的
  端口就是 bridge line，因此封锁一个端口是不够的。
- **计量式 bridge 分发。** `bridgedb` 从分桶中按每个
  邀请码分发少量 bridge；来自三个不同邀请的报告会让一个
  bridge 退役并补充分桶，因此枚举者消耗的是邀请，而不是
  网络。
- **无需任何 Nano 节点的引导。** 持有 bridge 秘密的客户端
  会获得一条小型免费线路，以便通过 bridge 访问账本并为
  自己充值。在隐身模式（stealth mode）下，客户端根本不联系 Nano 节点：它
  依据缓存的链状态离线签署付款，由入口 relay
  发布，此后的每一次账本调用都经由线路的出口进行。
- **受审查网络配置，始终开启。** 没有开关，也没有会被
  忘记的参数：优先选用 bridge 作为入口，从不从真实地址探测已登记的 relay，
  只从 bridge 获取 gossip，并且
  从不进行直接的账本调用。一个可以关闭的防御，对最需要它的人来说
  就是关闭着的。
- **没有可供封锁的单一列表。** relay 在 Nano 账本本身上注册，
  而账本无法被关停，relay 之间也相互 gossip 已签名的记录，因此
  拥有一个可用 bridge 的客户端无需账本即可了解整个网络。
  伪造记录需要 relay 的私钥。
- **DNS 从不离开设备。** 客户端在本地应答 DNS，并
  将每个查询经由线路转发到出口处的解析器；
  Android 应用和整机流量捕获会把每个域名导入黑洞（sinkhole）。
- **刻意不做阻断。** 没有 kill switch，也没有黑洞。
  如果 Android 客户端启动失败，隧道会被拆除，
  通知会说明原因，因此手机保留其普通连接，
  用户可以再次点击 Connect。一个悄悄夺走网络的隐私工具
  是人们会卸载的工具；而在有流量传输时，流量要么经过
  线路，要么根本不传输。
- **家用 relay。** NAT 之后的电脑以其港湾（harbour）所在的国家注册，不带
  ASN，并通过入站线路连接到港湾，因此账本从不
  显示运营者在哪里，同时也提供了一批住宅网络入口。
- **无法被冻结的支付。** 没有账户，没有代币，没有公司：一笔 XNO
  发送就是门票，任何人都可以运行 relay 并获得报酬。

这里的一切都尚未在受审查网络内部进行过测量；
测量装置和 bridge 分发服务将是完成这项工作的
工具。

### macOS："cannot be opened because the developer cannot be verified"

这条消息是 Gatekeeper 在报告该构建未签名，在仓库拥有
Apple Developer ID 证书之前都会如此。首次打开时请
右键 → 打开（或 系统设置 → 隐私与安全性 → 仍要打开）；
之后它会正常启动。要让所有人都不再看到这个警告，
需加入 Apple Developer Program（每年 99 美元，任何法律实体或
个人均可；仅有 sailnet.space 这样的域名本身是不够的），创建
一个 *Developer ID Application* 证书和一个 App 专用密码，并将
`MACOS_CERT_P12_BASE64`、`MACOS_CERT_PASSWORD`、`MACOS_SIGN_IDENTITY`、
`APPLE_ID`、`APPLE_TEAM_ID` 和 `APPLE_APP_PASSWORD` 添加为仓库 secrets。
这样，下一个 tag 就会自动签名并公证。Windows 也是同样的
情况，使用放在 `WINDOWS_CERT_PFX_BASE64` 中的代码签名证书。

## 工作原理

**线路。** 客户端构建一条伸缩式（telescoping）多跳线路（两到四个 relay，默认三个）：向
入口发送 CREATE，然后经由每个已建立的跳发送 EXTEND，每一跳
进行一次 X25519 握手。每个 cell 为 1 024 字节，每一跳各有一层 ChaCha20-Poly1305 加密，
在每个 relay 处剥去一层；nonce 是按跳、按方向的序列号，带有
64 个 cell 的防重放窗口，因此任何 relay 都无法重放或重排 cell 来标记
流量。入口看到客户端，出口看到目的地，没有人能同时看到
两者。线路内的流（stream）承载 TCP、UDP（数据报流，带长度分帧）
以及到出口的 DNS。

**支付。** 没有代币，没有账户：客户端向入口 relay 发送 XNO，
该发送的区块哈希就是线路标签。在隐身模式下，客户端从不
联系 Nano 节点；它依据缓存的链状态离线签署区块，并
交给入口，由入口发布并验证。relay 从汇集的发送中为下一跳
预付，并按配额计量每个 cell，因此没有人提供
信用，也没有什么可以封禁的。收入每 15
分钟转出到 `--payout`。

每一笔预付金额都是一定数量的*服务*，从来不是固定数目的 XNO：
一笔锚定款按入口公布的价格购买约 10 MiB，一个资金池按下一跳的价格购买
`--pool-mib`，而运营备用金是按对等节点公布的价格计算的八次充值
费用——从不按你自己的价格，因此为你的
relay 重新定价不会改变你何时拿到钱。各 relay 的价格不同，且会随时间变化，因此以
XNO 写定的金额，一旦发生其中任一情况，就买错了东西——一个
收费高于固定资金池所设定规模的 relay，过去会被悄悄跳过
而不是被付款，这就造成了网络分裂，而且任何地方都没有
错误信息。

**注册表。** relay 在 Nano 账本本身上注册：REGISTER 和
DESCRIPTOR 操作被编码在发送到国库账户（treasury account）的
状态区块的 representative 字段中，因此 relay 列表是公开的、可验证的，
并且不需要服务器。relay 还会对自己的记录签名并 gossip 它们，因此
只有一条 bridge line、无法访问账本的客户端仍然可以了解
网络；伪造记录需要 relay 的私钥。

**传输。** relay 是一个 HTTPS 站点。客户端发送当前 Chrome 的
ClientHello（uTLS），以 relay 的主机名作为 SNI，然后进行标准的 WebSocket
升级，其路径携带由 relay 密钥派生的每日令牌；任何其他
路径都会得到诱饵网站，探测请求会得到一个完全相同的 404。cell 承载于
真实的 RFC 6455 帧中，因此 bridge 可以置于支持 WebSocket 的 CDN 之后。
`--acme` 获取 Let's Encrypt 证书；CREATED 确认消息用 relay 的账本密钥
对当前 TLS 叶证书的 SHA-256 签名，因此伪造的证书会在
线路握手中被发现。

**流量整形，经过实测。** 隧道写入器以异步方式批处理 cell，
采用静默间隙合并（30 ms，上限为 250 ms 或 16 KiB），写入
满尺寸的 TLS 记录，暂缓不足一个记录的剩余部分，并依据真实 HTTPS 的
配置文件重放连接的最初几个记录。`sailtrace` 截取
TLS 之下的 TCP 流，按 DPI 设备的方式重建记录，在已公开的
TLS-in-TLS 特征集加上封装握手规则上训练随机森林，
并在实际网络上针对它调优参数。发布的默认值就是实测得到的那些。

**Bridge。** `relay --unlisted` 从不接触账本，并打印一个带有
秘密的 bridge line；令牌由密钥和秘密派生，因此读过
账本的探测者仍然只会得到诱饵。秘密的持有者会获得一条小型免费
引导线路来为自己充值。`bridgedb` 按每个邀请码分发少量 bridge，
并根据报告让已暴露的 bridge 退役。一个 relay 可以在
多个端口上提供服务；额外的端口就是 bridge line。

**家用节点。** `sailnode earn --home` 在 NAT 之后的电脑上运行 relay：
带重传的 NAT-PMP/PCP 和 UPnP、针对运营商级 NAT 的
公网 IP 真实性检查，以及当电脑无法被访问时，一条通往
公共 relay（它的港湾）的出站隧道，由港湾把线路桥接到它上面，注册表中
登记的是港湾所在的国家，因此账本从不显示运营者在哪里。

**奖励。** 每个 relay 都应把每天收入的 10% 分给其他 relay，其中 60%
按运行时长、40% 按表现分配，以带纪元标签（epoch-tagged）的发送点对点支付；
客户端根据账本重新计算该表，并通过排除不付款者的加权
抽签来选择路径。通过 `--levy` 选择加入。

**客户端。** 桌面：带远程 DNS 的 SOCKS5、经由线路转发的
DNS 解析器、可选的整机流量捕获（DNS sinkhole 加 Host/SNI
监听器）、仅浏览器扩展可读取的状态端点，以及一个在所有日志中
替代钱包地址和设备地址的昵称。
Android：带有用户态网络栈的 VpnService 把所有 TCP 和 UDP
经由线路路由，启动失败时会拆除隧道并给出原因，
而不是把手机的网络导入黑洞。Chrome：带有
WebRTC 防护的代理开关。

**隐私的局限，直言不讳。** Sailnet 无法隐藏设备在本地网络上的 MAC
地址或主机名，也无法隐藏公共账本上钱包的资金来源图谱；
如果 relay 和客户端钱包必须不能与你关联，请从交易所为它们充值。

## 浏览器扩展

`extension/` 是 Chrome 和 Firefox 共用的一套代码：一个代理开关，
把浏览器指向本地客户端带远程 DNS 的 SOCKS5 端口，一个
WebRTC 泄露防护，以及来自客户端状态端点的实时线路状态。
发布版附带 `sailnet-chrome.zip` 和 `sailnet-firefox.zip`。

- Chrome：`chrome://extensions`，开发者模式，对解压后的文件夹使用“加载已解压的扩展程序”
  （或在上架后从 Chrome Web Store 安装）。
- Firefox：`about:debugging`，对 zip 使用“临时载入附加组件”（或在上架后
  从 addons.mozilla.org 安装）。

`extension/build.sh` 打包两者。`Browser extensions` 工作流会对
Firefox 构建进行 lint，并且当商店凭据被设置为仓库
secrets 时（参见工作流文件头部），会在每个版本 tag 上传到 Chrome Web Store 并
提交到 addons.mozilla.org。

## 桌面应用

`sail/cmd/sailgui` 是 macOS、Windows 和 Linux 客户端：一个小窗口，
包含连接、钱包（地址、余额、在哪里获取 XNO）、状态和
设置（出口排除、bridge）。它运行与
`sailnode client` 相同的客户端，并在 127.0.0.1:1080 上提供 SOCKS5 代理，在
127.0.0.1:5300 上提供 DNS，供浏览器和扩展使用。发布版附带
`Sailnet-macOS-AppleSilicon.dmg`、`Sailnet-macOS-Intel.dmg` 和
`Sailnet-Windows.exe`。发布版中的每个文件旁都附有一个 `.sha256`。

当证书位于仓库 secrets 中时（macOS 需要 Developer ID 证书和 notarytool 凭据，
Windows 需要代码签名 PFX；参见工作流文件头部），`Desktop apps` 工作流会进行签名和公证。
没有它们时，构建是未签名的：macOS 首次需要右键 → 打开，
Windows 会显示 SmartScreen 的“仍要运行”。

本地构建：`cd sail && go build ./cmd/sailgui`（需要 C 编译器；在
Windows 上需要 MSYS2/mingw-w64）。

### macOS：一条命令安装并打开

```sh
arch=$(uname -m | sed 's/arm64/AppleSilicon/; s/x86_64/Intel/')
mnt=$(mktemp -d) \
  && curl -fsSL -o /tmp/Sailnet.dmg "https://github.com/dhyabi2/sailnet/releases/latest/download/Sailnet-macOS-$arch.dmg" \
  && curl -fsSL "https://github.com/dhyabi2/sailnet/releases/latest/download/Sailnet-macOS-$arch.dmg.sha256" | sed 's#  .*#  /tmp/Sailnet.dmg#' | shasum -a 256 -c - \
  && hdiutil attach -nobrowse -quiet -mountpoint "$mnt" /tmp/Sailnet.dmg \
  && rm -rf /Applications/Sailnet.app \
  && cp -R "$mnt/Sailnet.app" /Applications/ \
  && hdiutil detach -quiet "$mnt" \
  && open -a Sailnet
```

在替换任何东西之前，下载内容会与其旁边发布的 `.sha256` 进行校验，
就像上面 Linux 的一条命令校验二进制文件那样
（macOS 自带的是 `shasum`，而不是 `sha256sum`）。镜像被挂载到一个全新的
临时目录上，因此之前某个 DMG 遗留的、仍处于打开状态的 Sailnet 卷
不会被误复制。`rm -rf` 使这行命令既能干净地更新，
也能用于首次安装，因为在已有的 `Sailnet.app` 之上执行 `cp -R` 会把
旧版本的文件留在其中。如果 Sailnet 已在运行，请先退出它，
以便打开的是新版本。上文的右键 → 打开
适用于通过浏览器下载的文件；通过 `curl` 下载的文件不带
quarantine 属性，因此 Gatekeeper 不会询问，应用会自行打开，
顶部的按钮是 CONNECT。连接后它会开启
系统 SOCKS 代理，因此浏览器无需设置；终端和其他忽略系统代理的应用
可以指向 `socks5://127.0.0.1:1080`。

## 网站与品牌

`website/` 是静态站点及其两个小型 API 函数（`/api/stats`、
`/api/faucet`），部署在 Vercel 上的 [www.sailnet.space](https://www.sailnet.space)；
faucet 转发到其环境变量中指定的 relay。
`brand/` 存放标志和文字标识（SVG）以及应用图标，并附有规则：
仅黑白两色，不用渐变，不用圆角。

## 从源码构建

```
cd sail && go build -o bin/sailnode ./cmd/sailnode && go build -o bin/sail ./cmd/sail
go test ./...
```

从源码构建 Android APK（需要 Go、JDK 17、带 NDK 26 和
platform 34 的 Android SDK）：

```
go install golang.org/x/mobile/cmd/gomobile@latest golang.org/x/mobile/cmd/gobind@latest
export ANDROID_HOME=$HOME/Android/Sdk ANDROID_NDK_HOME=$ANDROID_HOME/ndk/26.3.11579264
cd sail && gomobile init && mkdir -p ../android/app/libs
gomobile bind -ldflags="-s -w" -target android/arm64,android/arm,android/amd64 -androidapi 24 \
  -javapkg net.sailnet -o ../android/app/libs/sail.aar ./mobile
cd ../android && ./gradlew assembleDebug
ls app/build/outputs/apk/debug/        # app-universal-debug.apk 以及按 ABI 划分的 APK
```

桌面应用：`cd sail && go install fyne.io/tools/cmd/fyne@latest && cd cmd/sailgui && fyne package -os darwin|windows|linux -icon Icon.png`。
同样的步骤在 `.github/workflows/android-release.yml` 和 `desktop.yml` 中运行。每次推送 `v*` tag 都会构建节点二进制文件（Linux x86-64 和 arm64）、桌面应用、浏览器扩展、Docker
镜像和 APK，并将它们附加到发布版中，每个都带有一个 `.sha256`。

## 目录结构

- `sail/` Go 实现：`nano/` 密钥与区块，`wire/` cell 与 onion
  加密层，`relay/` relay 服务器与线路客户端，`client/` 桌面客户端，
  `shape/` 流量整形及其测量装置，`cmd/` 二进制程序
- `android/`、`extension/` 各应用
- `docker/` 镜像与 compose 文件
