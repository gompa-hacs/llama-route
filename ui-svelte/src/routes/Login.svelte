<script lang="ts">
  import { login, authError } from "../stores/auth";

  let password = $state("");
  let loading = $state(false);
  let error = $state("");

  async function submit(e: Event) {
    e.preventDefault();
    loading = true;
    error = "";
    try {
      await login(password);
      password = "";
    } catch (err) {
      error = err instanceof Error ? err.message : "Login failed";
    } finally {
      loading = false;
    }
  }
</script>

<div class="min-h-screen flex items-center justify-center p-4">
  <form class="w-full max-w-sm space-y-4 border border-border rounded-lg p-6 bg-surface shadow" onsubmit={submit}>
    <h1 class="text-xl font-semibold">llama-swap login</h1>
    <p class="text-sm text-gray-600 dark:text-gray-400">Sign in to access the dashboard.</p>

    <label class="block space-y-1">
      <span class="text-sm">Admin password</span>
      <input
        class="w-full border border-border rounded px-3 py-2 bg-background"
        type="password"
        bind:value={password}
        autocomplete="current-password"
        required
      />
    </label>

    {#if error || $authError}
      <p class="text-sm text-red-600">{error || $authError}</p>
    {/if}

    <button
      class="w-full rounded bg-indigo-600 text-white py-2 disabled:opacity-50"
      type="submit"
      disabled={loading}
    >
      {loading ? "Signing in..." : "Sign in"}
    </button>
  </form>
</div>
