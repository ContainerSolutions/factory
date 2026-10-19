<script setup lang="ts">
import { computed, onMounted, ref } from 'vue';
import { RouterLink, RouterView, useRoute, useRouter } from 'vue-router';
import PauseControls from '@/components/controls/PauseControls.vue';
import { useSessionStore } from '@/stores/session';
import Message from '@/volt/Message.vue';
import SecondaryButton from '@/volt/SecondaryButton.vue';

const session = useSessionStore();
const route = useRoute();
const router = useRouter();

const nav = [
  { name: 'board', label: 'Board' },
  { name: 'pools', label: 'Pools' },
  { name: 'catalog', label: 'Catalog' },
] as const;

// The sign-in page stands alone, without the navigation.
const bare = computed(() => route.meta.public === true);

const signingOut = ref(false);
const signOutError = ref<string | null>(null);

async function signOut(): Promise<void> {
  signingOut.value = true;
  signOutError.value = null;
  try {
    const error = await session.signOut();
    if (error) signOutError.value = error;
    else await router.push({ name: 'login' });
  } finally {
    signingOut.value = false;
  }
}

onMounted(() => {
  // The router's session guard checks first; this only covers a shell mounted without it.
  session.ensure();
});
</script>

<template>
  <div
    class="min-h-screen bg-white text-ccf-900 dark:bg-ccf-950 dark:text-ccf-100"
  >
    <header
      class="flex flex-wrap items-center gap-x-8 gap-y-2 border-b border-ccf-200 px-6 py-3 dark:border-ccf-800"
    >
      <RouterLink :to="{ name: 'board' }" class="font-semibold"
        >CCF Factory</RouterLink
      >
      <nav v-if="!bare" aria-label="Main" class="flex gap-4 text-sm">
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
      <div v-if="!bare && session.me" class="ml-auto flex items-center gap-4">
        <PauseControls scope="global" />
        <span
          class="text-sm text-ccf-600 dark:text-ccf-400"
          data-testid="session-login"
        >
          {{ session.me.login }}
        </span>
        <span
          v-if="!session.canAct"
          class="text-xs text-ccf-500 dark:text-ccf-400"
          data-testid="read-only"
          >read-only</span
        >
        <SecondaryButton
          size="small"
          label="Sign out"
          :loading="signingOut"
          @click="signOut"
        />
      </div>
    </header>
    <Message
      v-if="signOutError"
      severity="error"
      class="mx-6 mt-4"
      role="alert"
    >
      Couldn't sign out: {{ signOutError }}
    </Message>
    <main class="px-6 py-6">
      <RouterView />
    </main>
  </div>
</template>
