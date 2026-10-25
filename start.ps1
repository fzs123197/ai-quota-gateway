$ErrorActionPreference = 'Stop'
[Console]::OutputEncoding = [System.Text.Encoding]::UTF8
$OutputEncoding = [System.Text.Encoding]::UTF8

function Write-Info([string]$Message) { Write-Host $Message -ForegroundColor Cyan }
function Write-Warn([string]$Message) { Write-Host $Message -ForegroundColor Yellow }
function Fail([string]$Message) { Write-Host $Message -ForegroundColor Red; exit 1 }

function Test-TcpPort([string]$HostName, [int]$Port, [int]$TimeoutMs = 500) {
    $client = New-Object System.Net.Sockets.TcpClient
    try {
        $async = $client.BeginConnect($HostName, $Port, $null, $null)
        if (-not $async.AsyncWaitHandle.WaitOne($TimeoutMs)) { return $false }
        $client.EndConnect($async)
        return $true
    } catch {
        return $false
    } finally {
        $client.Close()
    }
}

$Root = $PSScriptRoot
Set-Location $Root

$configPath = Join-Path $Root 'config.json'
$examplePath = Join-Path $Root 'config.example.json'
$createdConfig = $false
if (-not (Test-Path -LiteralPath $configPath)) {
    if (-not (Test-Path -LiteralPath $examplePath)) {
        Fail "缺少 config.json 和 config.example.json。"
    }
    Copy-Item -LiteralPath $examplePath -Destination $configPath
    $createdConfig = $true
}

try {
    $cfg = Get-Content -LiteralPath $configPath -Raw -Encoding UTF8 | ConvertFrom-Json
} catch {
    Fail "config.json 解析失败：$($_.Exception.Message)"
}

# 读取 Redis 地址（默认 127.0.0.1:6379）
$redisHost = '127.0.0.1'
$redisPort = 6379
$redisAddr = [string]$cfg.redis.addr
if ($redisAddr -match '^(?<host>.+):(?<port>\d+)$') {
    $redisHost = $Matches.host.Trim()
    $redisPort = [int]$Matches.port
}
if ($redisHost -eq '[::1]') { $redisHost = '::1' }
$localRedisHosts = @('127.0.0.1', 'localhost', '::1', '')
$isLocalRedis = $localRedisHosts -contains $redisHost.ToLowerInvariant()

# 缺 exe 时尝试构建
$exe = Join-Path $Root 'ai-quota-gateway.exe'
if (-not (Test-Path -LiteralPath $exe)) {
    Write-Info "未找到 ai-quota-gateway.exe，尝试从 src 构建..."
    $goExe = $null
    $goCmd = Get-Command go -ErrorAction SilentlyContinue
    if ($goCmd) { $goExe = $goCmd.Source }
    if (-not $goExe) {
        $candidate = Join-Path $env:USERPROFILE 'go-sdk\go\bin\go.exe'
        if (Test-Path -LiteralPath $candidate) { $goExe = $candidate }
    }
    if (-not $goExe) {
        Fail "未找到 Go 工具链，也没有 ai-quota-gateway.exe。请安装 Go 1.22+，或把编译好的 exe 放到根目录。"
    }
    & $goExe build -o $exe ./src
    if ($LASTEXITCODE -ne 0 -or -not (Test-Path -LiteralPath $exe)) {
        Fail "构建失败，请检查 src 目录和 Go 环境。"
    }
    Write-Info "构建完成：$exe"
}

if ($createdConfig) {
    Write-Warn "已从 config.example.json 生成 config.json。"
    Write-Warn "请先编辑 config.json，填写 gateway.api_key、gateway.admin_key 和 upstream[].api_key。"
}
$configRaw = Get-Content -LiteralPath $configPath -Raw -Encoding UTF8
if ($configRaw -match 'CHANGE-ME') {
    Write-Warn "config.json 仍包含 CHANGE-ME 占位符；面板可以启动，但调用上游会失败。"
}

$redisStarted = $false
$redisProc = $null
$redisCli = Join-Path $Root 'tools\redis\redis-cli.exe'
$redisServer = Join-Path $Root 'tools\redis\redis-server.exe'

try {
    if ($isLocalRedis) {
        if (Test-TcpPort $redisHost $redisPort 400) {
            Write-Info "检测到 $redisHost`:$redisPort 已有服务，复用它（脚本不会停止该服务）。"
        } else {
            if (-not (Test-Path -LiteralPath $redisServer)) {
                Fail "缺少便携 Redis：tools\redis\redis-server.exe。请重新克隆仓库，或安装 Memurai/Redis 并配置 redis.addr。"
            }
            $redisData = Join-Path $Root 'data\redis'
            New-Item -ItemType Directory -Force -Path $redisData | Out-Null
            $bindHost = if ($redisHost -eq '::1') { '::1' } else { '127.0.0.1' }
            $redisArgs = @(
                '--bind', $bindHost,
                '--port', "$redisPort",
                '--dir', '.',
                '--dbfilename', 'dump.rdb',
                '--save', '60', '1',
                '--appendonly', 'no',
                '--logfile', 'redis.log'
            )
            $redisProc = Start-Process -FilePath $redisServer -ArgumentList $redisArgs `
                -WorkingDirectory $redisData -WindowStyle Hidden -PassThru
            $redisStarted = $true

            $deadline = (Get-Date).AddSeconds(30)
            while (-not (Test-TcpPort $bindHost $redisPort 300)) {
                $redisProc.Refresh()
                if ($redisProc.HasExited -or (Get-Date) -gt $deadline) {
                    $logPath = Join-Path $redisData 'redis.log'
                    if (Test-Path -LiteralPath $logPath) {
                        Write-Host "--- redis.log（末尾） ---" -ForegroundColor DarkGray
                        Get-Content -LiteralPath $logPath -Tail 30
                    }
                    if (-not $redisProc.HasExited) { Stop-Process -Id $redisProc.Id -Force -ErrorAction SilentlyContinue }
                    $redisStarted = $false
                    Fail "Redis 启动失败，请检查端口 $redisPort 是否被占用，或查看 data\redis\redis.log。"
                }
                Start-Sleep -Milliseconds 300
            }
            Write-Info "Redis 已启动：$bindHost`:$redisPort（数据目录 data\redis）"
        }
    } else {
        Write-Info "Redis 配置为外部地址 $redisHost`:$redisPort，跳过本地 Redis 启动。"
    }

    $listen = [string]$cfg.gateway.listen_addr
    $gatewayPort = '8080'
    if ($listen -match ':(\d+)$') { $gatewayPort = $Matches[1] }
    Write-Info "正在启动网关：http://127.0.0.1:$gatewayPort"
    Write-Info "按 Ctrl+C 停止网关；本次脚本启动的 Redis 会随之停止。"
    Write-Host ""

    Push-Location $Root
    try {
        & $exe
        $code = $LASTEXITCODE
    } finally {
        Pop-Location
    }
    if ($null -eq $code) { $code = 0 }
} catch {
    Write-Host "启动失败：$($_.Exception.Message)" -ForegroundColor Red
    $code = 1
} finally {
    if ($redisStarted -and $redisProc) {
        Write-Info "正在停止 Redis..."
        if (Test-Path -LiteralPath $redisCli) {
            & $redisCli -h $redisHost -p $redisPort shutdown save 2>$null | Out-Null
        }
        Start-Sleep -Milliseconds 800
        $redisProc.Refresh()
        if (-not $redisProc.HasExited) {
            Stop-Process -Id $redisProc.Id -Force -ErrorAction SilentlyContinue
        }
    }
}

exit $code
