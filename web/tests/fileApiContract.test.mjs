/**
 * The other half of the file-API contract check.
 *
 * docs/api/file-api.schema.json is generated from the Go response types by
 * backend/internal/api/files_contract_schema_test.go. This test parses
 * web/src/api/client.ts and compares every interface it declares for a file-API resource
 * against that document: field names, optionality (`?` on the client, `omitempty` on the
 * server) and a coarse value kind.
 *
 * It is deliberately not a runtime test. The drift that matters is a field the server
 * stopped sending while the client still asserted it, and that disagreement lives in the
 * *types*, not in any request this suite happens to make against a mock. TypeScript erases
 * those types at runtime, so the syntax tree is the only place they can be read. Vite
 * already ships the parser this suite uses to load the other TypeScript modules, so this
 * adds no dependency.
 *
 * A rename on the Go side fails the Go test first (the checked-in document goes stale).
 * Regenerating the document then fails here until client.ts follows. Neither side can move
 * alone, which is the point: one fact, two directions.
 */
import assert from "node:assert/strict";
import { readFile } from "node:fs/promises";
import test from "node:test";
import { parseAst } from "vite";

const clientSource = await readFile(new URL("../src/api/client.ts", import.meta.url), "utf8");
const schema = JSON.parse(await readFile(new URL("../../docs/api/file-api.schema.json", import.meta.url), "utf8"));

const ast = parseAst(clientSource, { lang: "ts" });

/** Every interface and type alias client.ts declares, by name. */
const declarations = new Map();
for (const statement of ast.body) {
  if (statement.type !== "ExportNamedDeclaration" || !statement.declaration) continue;
  const declaration = statement.declaration;
  if (declaration.type === "TSInterfaceDeclaration" || declaration.type === "TSTypeAliasDeclaration") {
    declarations.set(declaration.id.name, declaration);
  }
}

const shapeCache = new Map();
const resolving = new Set();

function propertyName(member) {
  if (!member.key) return undefined;
  if (member.key.type === "Identifier") return member.key.name;
  if (member.key.type === "Literal") return String(member.key.value);
  return undefined;
}

function membersShape(members) {
  const fields = new Map();
  for (const member of members) {
    if (member.type !== "TSPropertySignature") continue;
    const name = propertyName(member);
    if (!name) continue;
    fields.set(name, { optional: Boolean(member.optional), kind: kindOf(member.typeAnnotation?.typeAnnotation) });
  }
  return fields;
}

function interfaceShape(node) {
  const fields = new Map();
  for (const heritage of node.extends ?? []) {
    const name = heritage.expression?.name;
    const declaration = name ? declarations.get(name) : undefined;
    if (!declaration) continue;
    for (const [field, spec] of declarationShape(declaration)) fields.set(field, spec);
  }
  for (const [field, spec] of membersShape(node.body.body)) fields.set(field, spec);
  return fields;
}

/**
 * Union alternatives describe one resource at different moments, so a property is required
 * only when every alternative requires it. FileChangeFilter is the useful case: the client
 * splits file and folder filters into two shapes while the server has one struct.
 */
function unionShape(alternatives) {
  const fields = new Map();
  for (const alternative of alternatives) {
    for (const [name, field] of alternative) {
      const seen = fields.get(name);
      if (!seen) {
        fields.set(name, { optional: true, kind: field.kind, members: 1, required: !field.optional });
        continue;
      }
      seen.members += 1;
      seen.required = seen.required && !field.optional;
      if (seen.kind !== field.kind) seen.kind = "any";
    }
  }
  for (const field of fields.values()) {
    field.optional = !(field.required && field.members === alternatives.length);
    delete field.members;
    delete field.required;
  }
  return fields;
}

function typeShape(node) {
  if (!node) return new Map();
  if (node.type === "TSUnionType") return unionShape(node.types.map(typeShape));
  if (node.type === "TSIntersectionType") {
    const fields = new Map();
    for (const member of node.types.map(typeShape)) for (const [name, spec] of member) fields.set(name, spec);
    return fields;
  }
  if (node.type === "TSParenthesizedType") return typeShape(node.typeAnnotation);
  if (node.type === "TSTypeLiteral") return membersShape(node.members);
  if (node.type === "TSTypeReference") {
    const name = node.typeName.type === "Identifier" ? node.typeName.name : node.typeName.right?.name;
    const declaration = name ? declarations.get(name) : undefined;
    return declaration ? declarationShape(declaration) : new Map();
  }
  return new Map();
}

function declarationShape(node) {
  if (shapeCache.has(node)) return shapeCache.get(node);
  shapeCache.set(node, new Map());
  const shape = node.type === "TSInterfaceDeclaration" ? interfaceShape(node) : typeShape(node.typeAnnotation);
  shapeCache.set(node, shape);
  return shape;
}

function literalKind(literal) {
  if (!literal) return "any";
  if (typeof literal.value === "string") return "string";
  if (typeof literal.value === "number") return "number";
  if (typeof literal.value === "boolean") return "boolean";
  return "any";
}

function declarationKind(node) {
  if (node.type === "TSInterfaceDeclaration") return "object";
  if (resolving.has(node)) return "any";
  resolving.add(node);
  try {
    return kindOf(node.typeAnnotation);
  } finally {
    resolving.delete(node);
  }
}

/** Coarse JSON kind of a property type; "any" means "do not compare". */
function kindOf(node) {
  if (!node) return "any";
  switch (node.type) {
    case "TSStringKeyword": return "string";
    case "TSNumberKeyword": return "number";
    case "TSBooleanKeyword": return "boolean";
    case "TSObjectKeyword": return "object";
    case "TSAnyKeyword":
    case "TSUnknownKeyword": return "any";
    case "TSArrayType": return "array";
    case "TSTypeLiteral": return "object";
    case "TSParenthesizedType": return kindOf(node.typeAnnotation);
    case "TSTypeOperator": return kindOf(node.typeAnnotation);
    case "TSIntersectionType": return "object";
    case "TSLiteralType": return literalKind(node.literal);
    case "TSUnionType": {
      const kinds = node.types.map(kindOf).filter((kind) => kind !== "null" && kind !== "any");
      return kinds.length > 0 && kinds.every((kind) => kind === kinds[0]) ? kinds[0] : "any";
    }
    case "TSTypeReference": {
      const name = node.typeName.type === "Identifier" ? node.typeName.name : node.typeName.right?.name;
      if (name === "Array" || name === "ReadonlyArray") return "array";
      if (name === "Record") return "object";
      const declaration = name ? declarations.get(name) : undefined;
      return declaration ? declarationKind(declaration) : "any";
    }
    default: return "any";
  }
}

test("the contract document covers every file resource the client declares", () => {
  assert.ok(Object.keys(schema.types).length > 0, "the contract document is empty");
  for (const name of Object.keys(schema.types)) {
    assert.ok(declarations.has(name), `docs/api/file-api.schema.json names ${name}, but web/src/api/client.ts does not declare it`);
  }
});

test("every file resource field, optionality and kind matches the Go response type", () => {
  for (const [name, resource] of Object.entries(schema.types)) {
    const declaration = declarations.get(name);
    assert.ok(declaration, `${name} is missing from web/src/api/client.ts`);
    const actual = declarationShape(declaration);
    const expected = resource.fields;
    assert.deepEqual(
      [...actual.keys()].sort(),
      Object.keys(expected).sort(),
      `${name}: fields differ between the Go response type and web/src/api/client.ts`,
    );
    for (const [field, spec] of Object.entries(expected)) {
      const got = actual.get(field);
      assert.equal(
        got.optional,
        spec.optional,
        `${name}.${field} optionality differs: server ${spec.optional ? "may omit it" : "always sends it"}, client ${got.optional ? "treats it as optional" : "requires it"}`,
      );
      if (spec.kind !== "any" && got.kind !== "any") {
        assert.equal(got.kind, spec.kind, `${name}.${field} has kind ${got.kind} in web/src/api/client.ts but ${spec.kind} on the server`);
      }
    }
  }
});
