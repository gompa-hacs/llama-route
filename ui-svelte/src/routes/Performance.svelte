<script lang="ts">
  import { onMount } from "svelte";
  import { fetchPerformance, fetchPeerMetrics, fetchPoolMetrics, poolMetrics } from "../stores/api";
  import { persistentStore } from "../stores/persistent";
  import type { SysStat, GpuStat, PeerSnapshot, PoolMetricsSnapshot } from "../lib/types";
  import PerformanceChart from "../components/PerformanceChart.svelte";

  const COLORS = [
    "#3b82f6",
    "#ef4444",
    "#10b981",
    "#f59e0b",
    "#8b5cf6",
    "#ec4899",
    "#06b6d4",
    "#84cc16",
    "#f97316",
    "#14b8a6",
    "#a855f7",
    "#e11d48",
    "#0ea5e9",
    "#eab308",
    "#d946ef",
    "#22d3ee",
  ];

  /** Stable series color from a numeric id or string key (order-independent). */
  function stableColor(key: string | number): string {
    if (typeof key === "number") {
      return COLORS[((key % COLORS.length) + COLORS.length) % COLORS.length];
    }
    let hash = 0;
    for (let i = 0; i < key.length; i++) {
      hash = (hash * 31 + key.charCodeAt(i)) | 0;
    }
    return COLORS[((hash % COLORS.length) + COLORS.length) % COLORS.length];
  }

  const WINDOWS = [
    { label: "5 min", ms: 5 * 60 * 1000 },
    { label: "15 min", ms: 15 * 60 * 1000 },
    { label: "1 hr", ms: 60 * 60 * 1000 },
  ] as const;

  const INTERVALS = [
    { label: "Off", ms: 0 },
    { label: "5s", ms: 5000 },
    { label: "10s", ms: 10000 },
    { label: "30s", ms: 30000 },
    { label: "60s", ms: 60000 },
  ] as const;

  let selectedWindow = persistentStore("perf-window", 0);
  let selectedInterval = persistentStore("perf-refresh-interval", 0);
  let sysData = $state<SysStat[]>([]);
  let gpuData = $state<GpuStat[]>([]);
  let refreshing = $state(false);

  let peerData = $state<Record<string, PeerSnapshot>>({});
  let peerPollTime = $state<string>("");

  let poolData = $state<PoolMetricsSnapshot | null>(null);

  async function loadPeerData() {
    const resp = await fetchPeerMetrics();
    if (resp) {
      peerData = resp.peers ?? {};
      peerPollTime = resp.poll_time ?? "";
    }
  }

  async function loadPoolData() {
    const resp = await fetchPoolMetrics();
    if (resp) {
      poolData = resp;
    }
  }

  let pollTimer: ReturnType<typeof setInterval> | null = null;
  let visible = $state(true);
  let mounted = $state(false);

  function cutoffTime(): number {
    return Date.now() - WINDOWS[$selectedWindow].ms;
  }

  function formatDelta(ts: string, refTime: number): string {
    const diffMs = new Date(ts).getTime() - refTime;
    const diffSec = Math.round(diffMs / 1000);
    const absSec = Math.abs(diffSec);
    const sign = diffSec <= 0 ? "-" : "+";
    if (absSec < 60) return `${sign}${absSec}s`;
    const min = Math.floor(absSec / 60);
    const sec = absSec % 60;
    if (sec === 0) return `${sign}${min}m`;
    return `${sign}${min}:${sec.toString().padStart(2, "0")}`;
  }

  const sysLabels = $derived.by(() => {
    const stats = filteredSysStats;
    if (stats.length === 0) return [];
    const refTime = new Date(stats[stats.length - 1].timestamp).getTime();
    return stats.map((s) => formatDelta(s.timestamp, refTime));
  });

  async function loadAll() {
    const resp = await fetchPerformance();
    if (resp) {
      sysData = resp.sys_stats ?? [];
      gpuData = resp.gpu_stats ?? [];
    }
    await loadPoolData();
  }

  async function loadIncremental() {
    const lastTs = sysData.length > 0 ? sysData[sysData.length - 1].timestamp : undefined;
    const resp = await fetchPerformance(lastTs);
    if (resp) {
      const newSys = resp.sys_stats ?? [];
      const newGpu = resp.gpu_stats ?? [];
      if (newSys.length > 0) {
        sysData = [...sysData, ...newSys];
      }
      if (newGpu.length > 0) {
        gpuData = [...gpuData, ...newGpu];
      }
    }
    await loadPoolData();
  }

  function startPolling() {
    stopPolling();
    const ms = INTERVALS[$selectedInterval].ms;
    if (ms <= 0) return;
    pollTimer = setInterval(() => {
      if (visible) {
        loadIncremental();
      }
    }, ms);
  }

  function stopPolling() {
    if (pollTimer) {
      clearInterval(pollTimer);
      pollTimer = null;
    }
  }

  function handleVisibility() {
    visible = !document.hidden;
    if (visible && mounted) {
      loadAll().then(() => startPolling());
    } else {
      stopPolling();
    }
  }

  function handleIntervalChange(i: number) {
    $selectedInterval = i;
    if (visible && mounted) {
      startPolling();
    }
  }

  async function manualRefresh() {
    refreshing = true;
    await loadIncremental();
    refreshing = false;
  }

  $effect(() => {
    return () => {
      stopPolling();
    };
  });

  onMount(() => {
    mounted = true;
    document.addEventListener("visibilitychange", handleVisibility);
    loadAll().then(() => startPolling());
    loadPeerData();

    const unsubPool = poolMetrics.subscribe((snap) => {
      if (snap) poolData = snap;
    });

    return () => {
      mounted = false;
      stopPolling();
      unsubPool();
      document.removeEventListener("visibilitychange", handleVisibility);
    };
  });

  // --- System charts (filtered by time window) ---

  const filteredSysStats = $derived(sysData.filter((s) => new Date(s.timestamp).getTime() >= cutoffTime()));

  const cpuDatasets = $derived.by(() => {
    const stats = filteredSysStats;
    if (stats.length === 0) return [];
    const coreCount = stats[0].cpu_util_per_core.length;
    const datasets = [];
    for (let i = 0; i < coreCount; i++) {
      datasets.push({
        label: `Core ${i}`,
        data: stats.map((s) => s.cpu_util_per_core[i]),
        borderColor: stableColor(i),
      });
    }
    return datasets;
  });

  const memSwapDatasets = $derived.by(() => {
    const stats = filteredSysStats;
    if (stats.length === 0) return [];
    return [
      {
        label: "Memory Used %",
        data: stats.map((s) => (s.mem_used_mb / s.mem_total_mb) * 100),
        borderColor: stableColor("mem-used"),
      },
      {
        label: "Swap Used %",
        data: stats.map((s) => (s.swap_total_mb > 0 ? (s.swap_used_mb / s.swap_total_mb) * 100 : 0)),
        borderColor: stableColor("swap-used"),
      },
    ];
  });

  const latestMemSwap = $derived.by(() => {
    const stats = filteredSysStats;
    if (stats.length === 0) return null;
    const s = stats[stats.length - 1];
    return {
      mem_total_mb: s.mem_total_mb,
      mem_used_mb: s.mem_used_mb,
      mem_used_pct: ((s.mem_used_mb / s.mem_total_mb) * 100).toFixed(1),
      swap_total_mb: s.swap_total_mb,
      swap_used_mb: s.swap_used_mb,
      swap_used_pct: s.swap_total_mb > 0 ? ((s.swap_used_mb / s.swap_total_mb) * 100).toFixed(1) : null,
    };
  });

  const loadDatasets = $derived.by(() => {
    const stats = filteredSysStats;
    if (stats.length === 0) return [];
    return [
      {
        label: "1 min",
        data: stats.map((s) => s.load_avg_1),
        borderColor: stableColor("load-1"),
      },
      {
        label: "5 min",
        data: stats.map((s) => s.load_avg_5),
        borderColor: stableColor("load-5"),
      },
      {
        label: "15 min",
        data: stats.map((s) => s.load_avg_15),
        borderColor: stableColor("load-15"),
      },
    ];
  });

  const netBandwidthDatasets = $derived.by(() => {
    const stats = filteredSysStats;
    if (stats.length < 2) return [];

    const ifaceNames = new Set<string>();
    for (const s of stats) {
      for (const n of s.net_io ?? []) {
        ifaceNames.add(n.name);
      }
    }

    const interfaces = [...ifaceNames].sort();
    if (interfaces.length === 0) return [];

    const datasets: { label: string; data: number[]; borderColor: string }[] = [];

    for (const iface of interfaces) {
      const recvData: number[] = [];
      const sentData: number[] = [];

      for (let i = 1; i < stats.length; i++) {
        const prev = stats[i - 1];
        const curr = stats[i];
        const prevIO = (prev.net_io ?? []).find((n) => n.name === iface);
        const currIO = (curr.net_io ?? []).find((n) => n.name === iface);

        if (!prevIO || !currIO) {
          recvData.push(0);
          sentData.push(0);
          continue;
        }

        const dtMs = new Date(curr.timestamp).getTime() - new Date(prev.timestamp).getTime();
        if (dtMs <= 0) {
          recvData.push(0);
          sentData.push(0);
          continue;
        }

        const dtSec = dtMs / 1000;
        recvData.push((((currIO.bytes_recv - prevIO.bytes_recv) / dtSec) * 8) / 1_000_000);
        sentData.push((((currIO.bytes_sent - prevIO.bytes_sent) / dtSec) * 8) / 1_000_000);
      }

      datasets.push({
        label: `${iface} in`,
        data: recvData,
        borderColor: stableColor(`${iface}:in`),
      });
      datasets.push({
        label: `${iface} out`,
        data: sentData,
        borderColor: stableColor(`${iface}:out`),
      });
    }

    return datasets;
  });

  const netBandwidthLabels = $derived.by(() => {
    const stats = filteredSysStats;
    if (stats.length < 2) return [];
    const refTime = new Date(stats[stats.length - 1].timestamp).getTime();
    return stats.slice(1).map((s) => formatDelta(s.timestamp, refTime));
  });

  // --- GPU charts (filtered by time window) ---

  const filteredGpuStats = $derived(gpuData.filter((g) => new Date(g.timestamp).getTime() >= cutoffTime()));

  const hasGpuData = $derived(gpuData.length > 0);

  const gpuLabels = $derived.by(() => {
    const seen = new Set<string>();
    const labels: string[] = [];
    const stats = filteredGpuStats;
    if (stats.length === 0) return [];
    const refTime = new Date(stats[stats.length - 1].timestamp).getTime();
    for (const g of stats) {
      const label = formatDelta(g.timestamp, refTime);
      if (!seen.has(label)) {
        seen.add(label);
        labels.push(label);
      }
    }
    return labels;
  });

  function buildGpuDatasets(
    stats: GpuStat[],
    field: keyof Pick<
      GpuStat,
      "gpu_util_pct" | "mem_util_pct" | "temp_c" | "vram_temp_c" | "power_draw_w" | "clock_mhz" | "mem_clock_mhz"
    >,
  ) {
    if (stats.length === 0) return [];

    const byId = new Map<number, { name: string; values: number[] }>();
    for (const g of stats) {
      if (!byId.has(g.id)) {
        byId.set(g.id, { name: g.name, values: [] });
      }
      byId.get(g.id)!.values.push(g[field] as number);
    }

    // Sort by id and key color off id so series keep the same color across refreshes
    // even when sample arrival order or which GPUs appear in the window changes.
    const datasets = [];
    for (const id of [...byId.keys()].sort((a, b) => a - b)) {
      const entry = byId.get(id)!;
      datasets.push({
        label: entry.name || `GPU ${id}`,
        data: entry.values,
        borderColor: stableColor(id),
      });
    }
    return datasets;
  }

  const gpuUtilDatasets = $derived(buildGpuDatasets(filteredGpuStats, "gpu_util_pct"));
  const gpuMemDatasets = $derived(buildGpuDatasets(filteredGpuStats, "mem_util_pct"));
  const gpuTempDatasets = $derived(buildGpuDatasets(filteredGpuStats, "temp_c"));
  const gpuVramTempDatasets = $derived(buildGpuDatasets(filteredGpuStats, "vram_temp_c"));
  const gpuPowerDatasets = $derived(buildGpuDatasets(filteredGpuStats, "power_draw_w"));
  const gpuClockDatasets = $derived(buildGpuDatasets(filteredGpuStats, "clock_mhz"));
  const gpuMemClockDatasets = $derived(buildGpuDatasets(filteredGpuStats, "mem_clock_mhz"));
  const hasVramTemp = $derived(filteredGpuStats.some((g) => g.vram_temp_c > 0));
  const hasGpuClock = $derived(filteredGpuStats.some((g) => g.clock_mhz > 0));
  const hasMemClock = $derived(filteredGpuStats.some((g) => g.mem_clock_mhz > 0));
</script>

<div class="space-y-6">
  <div class="flex items-center justify-between">
    <h2 class="text-xl font-semibold text-txtmain">Performance (Experimental)</h2>
    <div class="flex items-center gap-4">
      <div class="flex items-center gap-1">
        {#each WINDOWS as win, i}
          <button
            class="btn btn--sm"
            class:bg-primary={$selectedWindow === i}
            class:text-btn-primary-text={$selectedWindow === i}
            onclick={() => ($selectedWindow = i)}
          >
            {win.label}
          </button>
        {/each}
      </div>
      <div class="flex items-center gap-1">
        <span class="text-xs text-txtsecondary mr-1">Refresh:</span>
        {#each INTERVALS as intv, i}
          <button
            class="btn btn--sm"
            class:bg-primary={$selectedInterval === i}
            class:text-btn-primary-text={$selectedInterval === i}
            onclick={() => handleIntervalChange(i)}
          >
            {intv.label}
          </button>
        {/each}
      </div>
      <button class="btn btn--sm p-1" title="Refresh" onclick={manualRefresh} disabled={refreshing}>
        <svg
          xmlns="http://www.w3.org/2000/svg"
          viewBox="0 0 24 24"
          fill="none"
          stroke="currentColor"
          stroke-width="2"
          stroke-linecap="round"
          stroke-linejoin="round"
          class="w-4 h-4"
          class:animate-spin={refreshing}
        >
          <path d="M21 12a9 9 0 0 0-9-9 9.75 9.75 0 0 0-6.74 2.74L3 8" />
          <path d="M3 3v5h5" />
          <path d="M3 12a9 9 0 0 0 9 9 9.75 9.75 0 0 0 6.74-2.74L21 16" />
          <path d="M16 16h5v5" />
        </svg>
      </button>
    </div>
  </div>
  <p class="text-sm text-txtsecondary">
    This is an experimental feature. Please use <a
      class="underline hover:text-txtmain"
      href="https://github.com/mostlygeek/llama-swap/discussions/771">discussion #771</a
    > for instructions and to share feedback.
  </p>

  <!-- GPU Section -->
  <section class="space-y-4">
    <h3 class="text-lg font-medium text-txtmain">GPU</h3>
    {#if !hasGpuData}
      <p class="text-txtsecondary card p-4">No GPU data available</p>
    {:else}
      <div class="grid grid-cols-1 lg:grid-cols-2 gap-4">
        <PerformanceChart
          title="GPU Utilization (%)"
          labels={gpuLabels}
          datasets={gpuUtilDatasets}
          yMin={0}
          yMax={100}
          yLabel="%"
        />
        <PerformanceChart
          title="GPU Memory Utilization (%)"
          labels={gpuLabels}
          datasets={gpuMemDatasets}
          yMin={0}
          yMax={100}
          yLabel="%"
        />
        <PerformanceChart
          title="GPU Temperature (°C)"
          labels={gpuLabels}
          datasets={gpuTempDatasets}
          yMin={0}
          yLabel="°C"
        />
        {#if hasVramTemp}
          <PerformanceChart
            title="GPU VRAM Temperature (°C)"
            labels={gpuLabels}
            datasets={gpuVramTempDatasets}
            yMin={0}
            yLabel="°C"
          />
        {/if}
        <PerformanceChart
          title="GPU Power Draw (W)"
          labels={gpuLabels}
          datasets={gpuPowerDatasets}
          yMin={0}
          yLabel="W"
        />
        {#if hasGpuClock}
          <PerformanceChart
            title="GPU Clock (MHz)"
            labels={gpuLabels}
            datasets={gpuClockDatasets}
            yMin={0}
            yLabel="MHz"
          />
        {/if}
        {#if hasMemClock}
          <PerformanceChart
            title="GPU Memory Clock (MHz)"
            labels={gpuLabels}
            datasets={gpuMemClockDatasets}
            yMin={0}
            yLabel="MHz"
          />
        {/if}
      </div>
    {/if}
  </section>

  <!-- System Section -->
  <section class="space-y-4">
    <h3 class="text-lg font-medium text-txtmain">System</h3>
    <div class="grid grid-cols-1 lg:grid-cols-2 gap-4">
      <PerformanceChart
        title="CPU Utilization (%)"
        labels={sysLabels}
        datasets={cpuDatasets}
        yMin={0}
        yMax={100}
        yLabel="%"
        showLegend={false}
      />
      <div>
        <PerformanceChart
          title="Memory & Swap Usage (%)"
          labels={sysLabels}
          datasets={memSwapDatasets}
          yMin={0}
          yMax={100}
          yLabel="%"
        />
        {#if latestMemSwap}
          <div class="flex items-center justify-center gap-4 text-xs text-txtsecondary mt-1 px-4">
            <span
              >Mem: <span class="text-txtmain font-medium"
                >{latestMemSwap.mem_used_mb.toLocaleString()} / {latestMemSwap.mem_total_mb.toLocaleString()} MB ({latestMemSwap.mem_used_pct}%)</span
              ></span
            >
            {#if latestMemSwap.swap_used_pct !== null}
              <span
                >Swap: <span class="text-txtmain font-medium"
                  >{latestMemSwap.swap_used_mb.toLocaleString()} / {latestMemSwap.swap_total_mb.toLocaleString()} MB ({latestMemSwap.swap_used_pct}%)</span
                ></span
              >
            {/if}
          </div>
        {/if}
      </div>
      <PerformanceChart title="Load Average" labels={sysLabels} datasets={loadDatasets} yMin={0} />
      {#if netBandwidthDatasets.length > 0}
        <PerformanceChart
          title="Network Bandwidth (Mbit/s)"
          labels={netBandwidthLabels}
          datasets={netBandwidthDatasets}
          yMin={0}
          yLabel="Mbit/s"
          showLegend={false}
        />
      {/if}
    </div>
  </section>

  <!-- Pool Backend Load Section -->
  {#if poolData?.models?.length}
    <section class="space-y-4">
      <h3 class="text-lg font-medium text-txtmain">Pool Backends</h3>
      <p class="text-sm text-txtsecondary">
        Live load-balancer state for always-on upstream backends. Updated: {poolData.timestamp ? new Date(poolData.timestamp).toLocaleTimeString() : "never"}.
      </p>
      {#each poolData.models as model}
        <div class="space-y-2">
          <div class="flex items-baseline gap-2">
            <h4 class="font-medium text-txtmain">{model.model_id}</h4>
            <span class="text-xs text-txtsecondary">{model.strategy} · {model.affinity_sessions} sticky session{model.affinity_sessions === 1 ? "" : "s"}</span>
          </div>
          <div class="grid grid-cols-1 md:grid-cols-2 lg:grid-cols-3 gap-4">
            {#each model.backends as backend}
              <div class="card p-4 space-y-2">
                <div class="flex items-center justify-between gap-2">
                  <span class="font-medium text-txtmain truncate" title={backend.proxy}>#{backend.id} {backend.proxy}</span>
                  <span class="text-xs px-2 py-0.5 rounded-full shrink-0" class:bg-green-900={backend.healthy} class:text-green-300={backend.healthy} class:bg-red-900={!backend.healthy} class:text-red-300={!backend.healthy}>
                    {backend.healthy ? "healthy" : "down"}
                  </span>
                </div>
                <div class="text-xs text-txtsecondary space-y-1">
                  <p>Kind: {backend.kind ?? "static"}{#if backend.state} · {backend.state}{/if}</p>
                  <p>In-flight: {backend.inflight}</p>
                  <p>Sticky sessions: {backend.affinity_sessions}</p>
                  <p>Context size: {backend.context_size > 0 ? backend.context_size.toLocaleString() : "unknown"}</p>
                </div>
              </div>
            {/each}
          </div>
        </div>
      {/each}
    </section>
  {/if}

  <!-- Peer Performance Section -->
  {#if Object.keys(peerData).length > 0}
    <section class="space-y-4">
      <h3 class="text-lg font-medium text-txtmain">Peers</h3>
      <p class="text-sm text-txtsecondary">
        Metrics aggregated from upstream llama-swap sidecars. Last poll: {peerPollTime ? new Date(peerPollTime).toLocaleTimeString() : "never"}.
      </p>
      <div class="grid grid-cols-1 md:grid-cols-2 lg:grid-cols-3 gap-4">
        {#each Object.entries(peerData) as [, peer]}
          <div class="card p-4 space-y-3">
            <div class="flex items-center justify-between">
              <span class="font-medium text-txtmain">{peer.peer_name}</span>
              <span class="text-xs px-2 py-0.5 rounded-full" class:bg-green-900={peer.success} class:text-green-300={peer.success} class:bg-red-900={!peer.success} class:text-red-300={!peer.success}>
                {peer.success ? "connected" : "error"}
              </span>
            </div>
            {#if peer.error}
              <p class="text-xs text-red-400">{peer.error}</p>
            {/if}
            {#if peer.sys_stats?.length > 0}
              {@const latest = peer.sys_stats[peer.sys_stats.length - 1]}
              <div class="text-xs text-txtsecondary space-y-1">
                <p>CPU: {latest.cpu_util_per_core[0]?.toFixed(1) ?? "?"}%</p>
                <p>RAM: {((latest.mem_used_mb / latest.mem_total_mb) * 100).toFixed(1)}% ({latest.mem_used_mb} / {latest.mem_total_mb} MB)</p>
                <p>Load: {latest.load_avg_1.toFixed(2)} / {latest.load_avg_5.toFixed(2)} / {latest.load_avg_15.toFixed(2)}</p>
              </div>
            {/if}
            {#if peer.gpu_stats?.length > 0}
              <div class="text-xs text-txtsecondary space-y-1">
                {#each peer.gpu_stats as gpu}
                  <p>{gpu.name}: {gpu.gpu_util_pct.toFixed(1)}% util, {gpu.temp_c}°C, {((gpu.mem_used_mb / gpu.mem_total_mb) * 100).toFixed(1)}% VRAM{#if gpu.clock_mhz > 0}, {gpu.clock_mhz} MHz{/if}</p>
                {/each}
              </div>
            {/if}
            {#if peer.metrics}
              <p class="text-xs text-txtsecondary">{peer.metrics.length} requests recorded</p>
            {/if}
          </div>
        {/each}
      </div>
    </section>
  {/if}
</div>
