import type { OperationType, PlaygroundModel, PlaygroundOperation } from "./types";

export function operationOf(model: PlaygroundModel | undefined, type: OperationType): PlaygroundOperation | undefined {
  return model?.operations.find((operation) => operation.type === type);
}

export function modelsForOperation(models: PlaygroundModel[], type: OperationType): PlaygroundModel[] {
  return models.filter((model) => !model.external && Boolean(operationOf(model, type)));
}
