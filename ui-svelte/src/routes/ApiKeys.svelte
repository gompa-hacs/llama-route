<script lang="ts">
  import { onMount } from "svelte";
  import { authFetch } from "../stores/auth";
  import { inferenceApiKey } from "../stores/auth";

  interface PublicKey {
    id: string;
    name: string;
    prefix: string;
    created: string;
    lastUsed?: string;
    revoked?: boolean;
  }

  let keys = $state<PublicKey[]>([]);
  let newName = $state("");
  let createdSecret = $state("");
  let error = $state("");
  let loading = $state(false);

  async function loadKeys() {
    loading = true;
    error = "";
    try {
      const res = await authFetch("/api/admin/keys");
      if (!res.ok) throw new Error(`HTTP ${res.status}`);
      const data = await res.json();
      keys = data.keys ?? [];
    } catch (err) {
      error = String(err);
    } finally {
      loading = false;
    }
  }

  async function createKey() {
    error = "";
    createdSecret = "";
    try {
      const res = await authFetch("/api/admin/keys", {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({ name: newName || "unnamed" }),
      });
      if (!res.ok) throw new Error(`HTTP ${res.status}`);
      const data = await res.json();
      createdSecret = data.secret;
      newName = "";
      await loadKeys();
    } catch (err) {
      error = String(err);
    }
  }

  async function revokeKey(id: string) {
    error = "";
    try {
      const res = await authFetch(`/api/admin/keys/${id}`, { method: "DELETE" });
      if (!res.ok && res.status !== 204) throw new Error(`HTTP ${res.status}`);
      await loadKeys();
    } catch (err) {
      error = String(err);
    }
  }

  onMount(loadKeys);
</script>

<div class="max-w-3xl mx-auto space-y-6">
  <div>
    <h2 class="text-lg font-semibold">API keys</h2>
    <p class="text-sm text-gray-600 dark:text-gray-400">
      Create keys for inference clients. The secret is shown once when created.
    </p>
  </div>

  <div class="space-y-2 border border-border rounded p-4">
    <h3 class="font-medium">Playground / client key</h3>
    <p class="text-sm text-gray-600 dark:text-gray-400">
      Paste an inference API key here for playground requests from the browser.
    </p>
    <input
      class="w-full border border-border rounded px-3 py-2 bg-background font-mono text-sm"
      type="password"
      placeholder="sk-ls-..."
      bind:value={$inferenceApiKey}
    />
  </div>

  <div class="flex gap-2 items-end">
    <label class="flex-1 space-y-1">
      <span class="text-sm">New key name</span>
      <input class="w-full border border-border rounded px-3 py-2 bg-background" bind:value={newName} />
    </label>
    <button class="rounded bg-indigo-600 text-white px-4 py-2" type="button" onclick={createKey}>Create key</button>
  </div>

  {#if createdSecret}
    <div class="border border-amber-500 rounded p-3 bg-amber-50 dark:bg-amber-950 space-y-2">
      <p class="text-sm font-medium">Copy this key now — it will not be shown again:</p>
      <code class="block text-xs break-all">{createdSecret}</code>
    </div>
  {/if}

  {#if error}
    <p class="text-sm text-red-600">{error}</p>
  {/if}

  {#if loading}
    <p class="text-sm">Loading...</p>
  {:else}
    <table class="w-full text-sm border-collapse">
      <thead>
        <tr class="text-left border-b border-border">
          <th class="py-2">Name</th>
          <th class="py-2">Prefix</th>
          <th class="py-2">Created</th>
          <th class="py-2"></th>
        </tr>
      </thead>
      <tbody>
        {#each keys as key (key.id)}
          <tr class="border-b border-border">
            <td class="py-2">{key.name}{key.revoked ? " (revoked)" : ""}</td>
            <td class="py-2 font-mono">{key.prefix}</td>
            <td class="py-2">{new Date(key.created).toLocaleString()}</td>
            <td class="py-2 text-right">
              {#if !key.revoked}
                <button class="text-red-600 hover:underline" type="button" onclick={() => revokeKey(key.id)}>Revoke</button>
              {/if}
            </td>
          </tr>
        {/each}
      </tbody>
    </table>
  {/if}
</div>
