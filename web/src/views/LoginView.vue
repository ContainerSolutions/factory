<script setup lang="ts">
import { computed } from 'vue';
import { useRoute } from 'vue-router';
import { API_BASE } from '@/api/client';
import PageHeader from '@/components/shared/PageHeader.vue';
import { safeReturnTo } from '@/router/session';
import { useSessionStore } from '@/stores/session';
import Message from '@/volt/Message.vue';

const route = useRoute();
const session = useSessionStore();

// A page navigation, not a fetch: the dispatcher redirects to GitHub and, once signed in, sets
// its session cookie and redirects back to `returnTo`.
const href = computed(() => {
  const returnTo = safeReturnTo(route.query.returnTo);
  const query = returnTo === '/' ? '' : `?${new URLSearchParams({ returnTo })}`;
  return `${API_BASE}/auth/login${query}`;
});
</script>

<template>
  <div class="mx-auto mt-16 max-w-md">
    <PageHeader title="Sign in">
      CCF Factory uses your GitHub account. Members of the readers Group can see
      everything; maintainers can also pause, resume, cancel and re-dispatch.
    </PageHeader>
    <Message
      v-if="session.status === 'error'"
      severity="error"
      class="mb-4"
      role="alert"
    >
      Couldn't check your session: {{ session.error }}
    </Message>
    <a
      :href="href"
      class="inline-flex items-center justify-center rounded-md border border-primary bg-primary px-4 py-2 font-medium text-primary-contrast hover:bg-primary-emphasis focus-visible:outline focus-visible:outline-offset-2 focus-visible:outline-primary"
      data-testid="sign-in"
      >Sign in with GitHub</a
    >
  </div>
</template>
