import { defineStore } from 'pinia';
import { ref } from 'vue';
import { api, type Schemas } from '@/api/client';

/** The signed-in user, from `GET /auth/me`; null when signed out or not loaded yet. */
export const useSessionStore = defineStore('session', () => {
  const me = ref<Schemas['Me'] | null>(null);
  const loaded = ref(false);

  async function load(): Promise<void> {
    const { data } = await api.GET('/auth/me');
    me.value = data ?? null;
    loaded.value = true;
  }

  return { me, loaded, load };
});
