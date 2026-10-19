<script setup lang="ts">
import { onMounted } from 'vue';
import { RouterLink, RouterView } from 'vue-router';
import { useSessionStore } from '@/stores/session';

const session = useSessionStore();

const nav = [
  { name: 'board', label: 'Board' },
  { name: 'pools', label: 'Pools' },
  { name: 'catalog', label: 'Catalog' },
] as const;

onMounted(() => {
  session.load().catch(() => {
    // Signed out or unreachable: the shell still renders; sign-in comes with WEB-S1-T04.
  });
});
</script>

<template>
  <div
    class="min-h-screen bg-white text-ccf-900 dark:bg-ccf-950 dark:text-ccf-100"
  >
    <header
      class="flex items-center gap-8 border-b border-ccf-200 px-6 py-3 dark:border-ccf-800"
    >
      <RouterLink :to="{ name: 'board' }" class="font-semibold"
        >CCF Factory</RouterLink
      >
      <nav aria-label="Main" class="flex gap-4 text-sm">
        <RouterLink
          v-for="item in nav"
          :key="item.name"
          :to="{ name: item.name }"
          class="rounded px-2 py-1 text-ccf-600 hover:bg-ccf-100 dark:text-ccf-400 dark:hover:bg-ccf-800"
          exact-active-class="bg-ccf-100 text-ccf-900 dark:bg-ccf-800 dark:text-ccf-100"
        >
          {{ item.label }}
        </RouterLink>
      </nav>
      <span
        v-if="session.me"
        class="ml-auto text-sm text-ccf-600 dark:text-ccf-400"
        data-testid="session-login"
      >
        {{ session.me.login }}
      </span>
    </header>
    <main class="px-6 py-6">
      <RouterView />
    </main>
  </div>
</template>
