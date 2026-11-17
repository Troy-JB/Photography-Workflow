import { createRouter, createWebHashHistory } from 'vue-router'
import { resolveAuthNavigation } from './auth-navigation'
import ArchiveListView from './views/ArchiveListView.vue'
import ArchiveView from './views/ArchiveView.vue'
import DeliveryDetailView from './views/DeliveryDetailView.vue'
import DeliveryListView from './views/DeliveryListView.vue'
import CommerceView from './views/CommerceView.vue'
import GalleryDetailView from './views/GalleryDetailView.vue'
import GalleryListView from './views/GalleryListView.vue'
import LoginView from './views/LoginView.vue'
import LeadListView from './views/LeadListView.vue'
import RegisterView from './views/RegisterView.vue'
import ProofDetailView from './views/ProofDetailView.vue'
import ProofListView from './views/ProofListView.vue'
import RetentionSettingsView from './views/RetentionSettingsView.vue'
import ScheduleView from './views/ScheduleView.vue'
import WorkbenchView from './views/WorkbenchView.vue'
import ClientListView from './views/ClientListView.vue'
import ClientWorkflowView from './views/ClientWorkflowView.vue'
import LumeoHomeView from './views/LumeoHomeView.vue'

// Hash routing keeps the console deployable as static files without any
// server-side rewrite rules.
export const router = createRouter({
  history: createWebHashHistory(),
  routes: [
    { path: '/', name: 'home', component: LumeoHomeView },
    { path: '/workbench', name: 'workbench', component: WorkbenchView },
    { path: '/clients', name: 'clients', component: ClientListView },
    { path: '/clients/:id', name: 'client-workflow', component: ClientWorkflowView, props: true },
    { path: '/login', name: 'login', component: LoginView },
    { path: '/register', name: 'register', component: RegisterView },
    { path: '/galleries', name: 'galleries', component: GalleryListView },
    { path: '/g/:id', name: 'gallery', component: GalleryDetailView, props: true },
    { path: '/proofs', name: 'proofs', component: ProofListView },
    { path: '/leads', name: 'leads', component: LeadListView },
    { path: '/schedule', name: 'schedule', component: ScheduleView },
    { path: '/commerce', name: 'commerce', component: CommerceView },
    { path: '/p/:id', name: 'proof', component: ProofDetailView, props: true },
    { path: '/deliveries', name: 'deliveries', component: DeliveryListView },
    { path: '/deliveries/:id', name: 'delivery', component: DeliveryDetailView, props: true },
    { path: '/archives', name: 'archives', component: ArchiveListView },
    { path: '/archives/:id', name: 'archive', component: ArchiveView, props: true },
    { path: '/settings/retention', name: 'retention', component: RetentionSettingsView },
    { path: '/:pathMatch(.*)*', redirect: '/' },
  ],
  scrollBehavior: () => ({ top: 0 }),
})

router.beforeEach(resolveAuthNavigation)
