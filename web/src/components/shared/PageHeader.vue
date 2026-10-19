<script setup lang="ts">
defineProps<{ title: string }>();
</script>

<template>
  <header class="mb-6">
    <h1 class="text-2xl font-semibold text-ccf-900 dark:text-ccf-100">
      {{ title }}
    </h1>
    <p
      v-if="$slots.default"
      class="mt-1 text-sm text-ccf-600 dark:text-ccf-400"
    >
      <slot />
    </p>
  </header>
</template>
