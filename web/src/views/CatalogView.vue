<script setup lang="ts">
import { computed, onMounted, ref } from 'vue';
import CatalogStatus from '@/components/catalog/CatalogStatus.vue';
import DryRunPanel from '@/components/catalog/DryRunPanel.vue';
import ProviderTable from '@/components/catalog/ProviderTable.vue';
import QueuesAndPools from '@/components/catalog/QueuesAndPools.vue';
import RuleSetCard from '@/components/catalog/RuleSetCard.vue';
import TaskSpecTable from '@/components/catalog/TaskSpecTable.vue';
import PageHeader from '@/components/shared/PageHeader.vue';
import { useCatalogStore } from '@/stores/catalog';
import Message from '@/volt/Message.vue';
import SecondaryButton from '@/volt/SecondaryButton.vue';

const store = useCatalogStore();

const providerObjects = computed(() => store.providers.map((v) => v.provider));
const resyncRequested = ref(false);

onMounted(() => {
  store.load();
});

function resynced(): void {
  resyncRequested.value = true;
}

async function reload(): Promise<void> {
  resyncRequested.value = false;
  await store.load({ quiet: true });
}
</script>

<template>
  <PageHeader title="Catalog">
    The configuration the dispatcher runs: providers, RuleSets and the rules
    they select, TaskSpecs, queues and pools.
  </PageHeader>

  <p
    v-if="store.loading"
    class="mb-4 text-sm text-ccf-600 dark:text-ccf-400"
    role="status"
  >
    Loading…
  </p>

  <template v-else>
    <Message
      v-if="store.summaryError"
      severity="error"
      class="mb-4"
      role="alert"
    >
      Couldn't load the catalog: {{ store.summaryError }}
    </Message>
    <Message
      v-if="resyncRequested"
      severity="info"
      class="mb-4"
      role="status"
      data-testid="resync-requested"
    >
      <span class="flex flex-wrap items-center gap-3">
        Resync requested: the dispatcher re-reads the catalog in the background.
        <SecondaryButton size="small" label="Reload" @click="reload" />
      </span>
    </Message>
    <CatalogStatus
      v-if="store.summary"
      :summary="store.summary"
      @resynced="resynced"
    />
    <Message
      v-if="store.objectErrors.length"
      severity="warn"
      class="mb-4"
      role="status"
    >
      Some objects didn't load:
      <ul class="mt-1 list-disc ps-5">
        <li v-for="e in store.objectErrors" :key="e">{{ e }}</li>
      </ul>
    </Message>

    <section aria-labelledby="providers-heading" class="mb-8">
      <h2 id="providers-heading" class="mb-3 text-lg font-semibold">
        Providers
      </h2>
      <Message
        v-if="store.providersError"
        severity="error"
        class="mb-4"
        role="alert"
      >
        Couldn't load the providers: {{ store.providersError }}
      </Message>
      <ProviderTable
        v-if="store.providers.length"
        :providers="store.providers"
      />
      <p
        v-else-if="store.providersLoaded"
        class="text-sm text-ccf-500 dark:text-ccf-400"
      >
        No providers.
      </p>
    </section>

    <template v-if="store.summary">
      <section aria-labelledby="rulesets-heading" class="mb-8">
        <h2 id="rulesets-heading" class="mb-3 text-lg font-semibold">
          RuleSets
        </h2>
        <div v-if="store.ruleSets.length" class="grid gap-4 xl:grid-cols-2">
          <RuleSetCard
            v-for="rs in store.ruleSets"
            :key="rs.metadata.name"
            :rule-set="rs"
            :rules="store.rules"
            :providers="providerObjects"
          />
        </div>
        <p v-else class="text-sm text-ccf-500 dark:text-ccf-400">
          No RuleSets.
        </p>
      </section>

      <section aria-labelledby="taskspecs-heading" class="mb-8">
        <h2 id="taskspecs-heading" class="mb-3 text-lg font-semibold">
          TaskSpecs
        </h2>
        <TaskSpecTable
          v-if="store.taskSpecs.length"
          :task-specs="store.taskSpecs"
        />
        <p v-else class="text-sm text-ccf-500 dark:text-ccf-400">
          No TaskSpecs.
        </p>
      </section>

      <section aria-labelledby="queues-pools-heading" class="mb-8">
        <h2 id="queues-pools-heading" class="mb-3 text-lg font-semibold">
          Queues and pools
        </h2>
        <QueuesAndPools :queues="store.queues" :pools="store.pools" />
      </section>

      <section
        v-if="store.namesOf('Group').length || store.namesOf('Template').length"
        aria-labelledby="other-heading"
        class="mb-8"
      >
        <h2 id="other-heading" class="mb-3 text-lg font-semibold">
          Groups and Templates
        </h2>
        <dl class="grid grid-cols-[max-content_1fr] gap-x-4 gap-y-1 text-sm">
          <dt class="text-ccf-600 dark:text-ccf-400">Groups</dt>
          <dd class="font-mono text-xs leading-5" data-testid="groups">
            {{ store.namesOf('Group').join(', ') || 'none' }}
          </dd>
          <dt class="text-ccf-600 dark:text-ccf-400">Templates</dt>
          <dd class="font-mono text-xs leading-5">
            {{ store.namesOf('Template').join(', ') || 'none' }}
          </dd>
        </dl>
      </section>
    </template>

    <DryRunPanel
      class="border-t border-ccf-200 pt-6 dark:border-ccf-800"
      :catalog-sha="store.summary?.sha"
    />
  </template>
</template>
