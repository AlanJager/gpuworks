/** What an external platform hands over to become compute nodes (provider.Resource). */
export type PlatformResource = "vm" | "container" | "host" | "kubernetes" | "slurm";

/** A resource a provider kind connects, with the vendor's own product name for it when it has one (ECS, ACK). */
export interface KindResource {
  type: PlatformResource;
  product?: string;
}
