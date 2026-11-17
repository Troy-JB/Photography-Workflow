<script setup lang="ts">
import { computed } from 'vue'
import { useRoute } from 'vue-router'
import AppShell from './components/AppShell.vue'
import ToastHost from './components/ToastHost.vue'

const route = useRoute()
const bare = computed(() => route.name === 'home' || route.name === 'login' || route.name === 'register')
</script>

<template>
  <RouterView v-if="bare" />
  <AppShell v-else>
    <RouterView />
  </AppShell>
  <ToastHost />
</template>
