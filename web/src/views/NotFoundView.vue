<script setup lang="ts">
import { useRouter } from 'vue-router';
import PageHeader from '@/components/shared/PageHeader.vue';
import Button from '@/volt/Button.vue';

const router = useRouter();
</script>

<template>
  <PageHeader title="Not found">There is no page at this address.</PageHeader>
  <Button label="Go to the board" @click="router.push({ name: 'board' })" />
</template>
