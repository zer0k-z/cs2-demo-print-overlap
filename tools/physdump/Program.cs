// Dumps the world collision of a CS2 map as triangles, for the press height check in the Go tool.
//
// Usage: physdump <map.vpk> <entry path inside the vpk> <output file>
//
// Output (little endian):
//   int32 attributeCount
//   attributeCount x string   interact-as tags joined with ',' then '|' and the collision group
//                             (.NET BinaryWriter string: 7-bit encoded length + UTF-8)
//   int32 triangleCount
//   triangleCount x { int32 attributeIndex, 9 x float32 vertex positions }

using System.Numerics;
using SteamDatabase.ValvePak;
using ValveResourceFormat;
using ValveResourceFormat.ResourceTypes;
using ValveResourceFormat.Serialization.KeyValues;

if (args.Length != 3)
{
    Console.Error.WriteLine("Usage: physdump <map.vpk> <entry path> <output file>");
    return 1;
}

using var package = new Package();
package.Read(args[0]);
var entry = package.FindEntry(args[1]);
if (entry == null)
{
    Console.Error.WriteLine($"{args[1]} not found in {args[0]}");
    return 1;
}
package.ReadEntry(entry, out var bytes);

using var resource = new Resource();
resource.Read(new MemoryStream(bytes));
var phys = (resource.DataBlock as Model)?.GetEmbeddedPhys();
if (phys == null)
{
    Console.Error.WriteLine($"{args[1]} has no physics data");
    return 1;
}

var triangles = new List<(int Attribute, Vector3 A, Vector3 B, Vector3 C)>();
for (var p = 0; p < phys.Parts.Length; p++)
{
    var shape = phys.Parts[p].Shape;
    var pose = phys.BindPose.Length == 0 ? Matrix4x4.Identity : phys.BindPose[p];

    foreach (var hull in shape.Hulls)
    {
        var vertices = hull.Shape.GetVertexPositions().ToArray().Select(v => Vector3.Transform(v, pose)).ToArray();
        var edges = hull.Shape.GetEdges();
        foreach (var face in hull.Shape.GetFaces())
        {
            // Walk the face's half edge loop and triangulate it as a fan.
            var loop = new List<int>();
            var edge = (int)face.Edge;
            do
            {
                loop.Add(edges[edge].Origin);
                edge = edges[edge].Next;
            }
            while (edge != face.Edge && loop.Count <= edges.Length);
            for (var i = 1; i + 1 < loop.Count; i++)
            {
                triangles.Add((hull.CollisionAttributeIndex, vertices[loop[0]], vertices[loop[i]], vertices[loop[i + 1]]));
            }
        }
    }

    foreach (var mesh in shape.Meshes)
    {
        var vertices = mesh.Shape.GetVertices().ToArray().Select(v => Vector3.Transform(v, pose)).ToArray();
        foreach (var t in mesh.Shape.GetTriangles())
        {
            triangles.Add((mesh.CollisionAttributeIndex, vertices[t.X], vertices[t.Y], vertices[t.Z]));
        }
    }
}

Directory.CreateDirectory(Path.GetDirectoryName(Path.GetFullPath(args[2]))!);
var tempPath = args[2] + ".tmp";
using (var writer = new BinaryWriter(File.Create(tempPath)))
{
    writer.Write(phys.CollisionAttributes.Count);
    foreach (var attributes in phys.CollisionAttributes)
    {
        // Older assets carry the tags under m_PhysicsTagStrings.
        var tags = string.Join(",", attributes.GetArray<string>("m_InteractAsStrings")
            ?? attributes.GetArray<string>("m_PhysicsTagStrings")
            ?? []);
        var group = attributes.GetStringProperty("m_CollisionGroupString") ?? "";
        writer.Write(tags + "|" + group);
    }
    writer.Write(triangles.Count);
    foreach (var (attribute, a, b, c) in triangles)
    {
        writer.Write(attribute);
        foreach (var v in new[] { a, b, c })
        {
            writer.Write(v.X);
            writer.Write(v.Y);
            writer.Write(v.Z);
        }
    }
}
File.Move(tempPath, args[2], true);
Console.WriteLine($"Wrote {triangles.Count} triangles to {args[2]}");
return 0;
