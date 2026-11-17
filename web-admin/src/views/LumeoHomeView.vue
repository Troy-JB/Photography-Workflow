<script setup lang="ts">
import { onMounted, onUnmounted } from 'vue'

let observer: IntersectionObserver | undefined

onMounted(() => {
  const root = document.getElementById('lumeo-site')
  const targets = root?.querySelectorAll<HTMLElement>('[data-reveal]') ?? []
  if (window.matchMedia('(prefers-reduced-motion: reduce)').matches || !('IntersectionObserver' in window)) {
    targets.forEach((target) => target.classList.add('is-visible'))
    return
  }
  observer = new IntersectionObserver((entries) => {
    for (const entry of entries) {
      if (!entry.isIntersecting) continue
      entry.target.classList.add('is-visible')
      observer?.unobserve(entry.target)
    }
  }, { threshold: 0.14 })
  targets.forEach((target) => observer?.observe(target))
})

onUnmounted(() => observer?.disconnect())

function scrollToSection(id: string): void {
  document.getElementById(id)?.scrollIntoView({ behavior: 'smooth', block: 'start' })
}
</script>

<template>
  <main id="lumeo-site">
    <section id="home" class="hero" aria-labelledby="hero-title">
      <div class="hero-photo" role="img" aria-label="Editorial portrait in warm studio light" />
      <div class="hero-shade" />
      <header class="site-header">
        <a class="wordmark" href="#home" aria-label="Lumeo home" @click.prevent="scrollToSection('home')">Lumeo<span>.</span></a>
        <nav class="site-nav" aria-label="Main navigation">
          <a href="#home" @click.prevent="scrollToSection('home')">Home</a>
          <a href="#services" @click.prevent="scrollToSection('services')">Services</a>
          <a href="#portfolio" @click.prevent="scrollToSection('portfolio')">Portfolio</a>
          <a href="#about" @click.prevent="scrollToSection('about')">About</a>
        </nav>
        <div class="nav-actions">
          <RouterLink class="nav-login" to="/login">Log in</RouterLink>
          <RouterLink class="nav-register" to="/register">Register <span aria-hidden="true">↗</span></RouterLink>
        </div>
      </header>

      <div class="hero-kicker" data-reveal><span class="live-dot" /> Portrait studio <span class="kicker-divider">/</span> Available for select projects</div>
      <div class="hero-title-wrap" data-reveal>
        <p id="hero-title" class="hero-title" aria-label="Lumeo">LUMEO</p>
        <div class="hero-title-caption"><span>Portraits, made personal</span><span>01 — 04</span></div>
      </div>

      <div class="hero-bottom">
        <div class="hero-copy" data-reveal>
          <p class="slogan">Light up your<br />portrait service.</p>
          <p class="hero-description">Portraits with presence. Thoughtful direction, honest connection, and light that feels like you.</p>
        </div>
        <div class="contact-panel" data-reveal>
          <div class="contact-heading">
            <span class="contact-index">GET STARTED</span>
            <p>Launch your portrait workflow</p>
          </div>
          <p class="start-description">Streamline shooting, retouching and delivery in one unified platform.</p>
          <RouterLink class="start-button" to="/register">Start now <span aria-hidden="true">→</span></RouterLink>
        </div>
      </div>
      <a class="scroll-cue" href="#services" @click.prevent="scrollToSection('services')"><span /> Scroll to explore</a>
    </section>

    <section id="services" class="services-section section-pad">
      <div class="section-label" data-reveal><span>01</span> WHAT WE DO</div>
      <div class="services-grid">
        <h2 data-reveal>Portraits that<br /><em>feel like you.</em></h2>
        <p class="section-intro" data-reveal>From the first conversation to the final frame, every detail is shaped around the person in front of the lens.</p>
      </div>
      <div class="service-list" data-reveal>
        <div><span>01</span><strong>Personal portraits</strong><span>Character, captured with care</span><b>↗</b></div>
        <div><span>02</span><strong>Editorial sessions</strong><span>Ideas brought into focus</span><b>↗</b></div>
        <div><span>03</span><strong>Portrait direction</strong><span>At ease, from the first frame</span><b>↗</b></div>
      </div>
    </section>

    <section id="portfolio" class="portfolio-section section-pad">
      <div class="section-label" data-reveal><span>02</span> A STUDY IN LIGHT</div>
      <div class="portfolio-heading" data-reveal><h2>People, in<br /><em>their own light.</em></h2><span>Editorial studies<br />LUMEO STUDIO</span></div>
      <div class="portfolio-grid">
        <figure class="portfolio-frame frame-one" data-reveal><div class="portfolio-image portrait-one" role="img" aria-label="Editorial portrait study in warm light" /><figcaption><span>01 / Soft focus</span><span>Portrait study</span></figcaption></figure>
        <figure class="portfolio-frame frame-two" data-reveal><div class="portfolio-image portrait-two" role="img" aria-label="Editorial portrait study in deep shadow" /><figcaption><span>02 / Afterglow</span><span>Portrait study</span></figcaption></figure>
        <figure class="portfolio-frame frame-three" data-reveal><div class="portfolio-image portrait-three" role="img" aria-label="Editorial portrait study in window light" /><figcaption><span>03 / Window light</span><span>Portrait study</span></figcaption></figure>
      </div>
    </section>

    <section id="about" class="about-section section-pad">
      <div class="section-label" data-reveal><span>03</span> THE LUMEO APPROACH</div>
      <div class="about-grid">
        <h2 data-reveal>Good portraits<br />begin <em>with trust.</em></h2>
        <div data-reveal><p>We make room for the real you: a calm process, a considered eye, and photographs that hold onto how it felt to be there.</p><RouterLink class="text-link" to="/register">Start your workflow <span>↗</span></RouterLink></div>
      </div>
      <footer class="site-footer"><a class="wordmark" href="#home" @click.prevent="scrollToSection('home')">Lumeo<span>.</span></a><span>Light up your portrait service.</span><RouterLink to="/register">Get started ↗</RouterLink><small>© 2026 Lumeo Studio</small></footer>
    </section>
  </main>
</template>

<style scoped src="../lumeo.css"></style>
