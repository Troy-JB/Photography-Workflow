// Helpers for the manual revision upload. The checksum is computed in the
// browser so a corrupt transfer is rejected before it becomes a stored version.
export function stripExtension(name: string): string {
  const dot = name.lastIndexOf('.')
  return (dot > 0 ? name.slice(0, dot) : name).trim()
}

export async function sha256Hex(file: Blob): Promise<string> {
  const buffer = await file.arrayBuffer()
  const digest = await crypto.subtle.digest('SHA-256', buffer)
  return Array.from(new Uint8Array(digest))
    .map((byte) => byte.toString(16).padStart(2, '0'))
    .join('')
}

export async function imageSize(file: Blob): Promise<{ width: number; height: number }> {
  if (typeof createImageBitmap === 'function') {
    try {
      const bitmap = await createImageBitmap(file)
      const size = { width: bitmap.width, height: bitmap.height }
      bitmap.close()
      return size
    } catch {
      // Fall through to unknown dimensions; the server accepts 0/0.
    }
  }
  return { width: 0, height: 0 }
}

export async function compressImageForUpload(file: File, maxBytes: number): Promise<File> {
  if (file.size <= maxBytes) return file
  if (typeof createImageBitmap !== 'function') {
    throw new Error('当前浏览器不支持大图片自动压缩，请改用不超过 50 MB 的图片')
  }

  const bitmap = await createImageBitmap(file)
  try {
    let scale = 1
    let quality = 0.86
    for (let attempt = 0; attempt < 12; attempt += 1) {
      const canvas = document.createElement('canvas')
      canvas.width = Math.max(1, Math.round(bitmap.width * scale))
      canvas.height = Math.max(1, Math.round(bitmap.height * scale))
      const context = canvas.getContext('2d')
      if (!context) throw new Error('当前浏览器无法创建图片压缩画布')
      context.drawImage(bitmap, 0, 0, canvas.width, canvas.height)
      const blob = await new Promise<Blob | null>((resolve) => canvas.toBlob(resolve, 'image/webp', quality))
      if (blob && blob.size <= maxBytes) {
        const name = file.name.replace(/\.[^.]+$/, '') + '.webp'
        return new File([blob], name, { type: 'image/webp', lastModified: file.lastModified })
      }
      if (attempt % 2 === 1) scale *= 0.82
      else quality = Math.max(0.42, quality - 0.08)
    }
  } finally {
    bitmap.close()
  }
  throw new Error('图片压缩后仍超过 50 MB，请手动缩小图片后重试')
}
